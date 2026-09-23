// Package hls 实现 MV 的 HLS 转码（原唱/伴唱多音轨、渐进式播放、缓存清理）。
//
// 方案要点（与原 Node.js 版 hlsgen.js 一致）：
//   - 视频轨与每条音频轨分别独立切片，master.m3u8 通过 EXT-X-MEDIA 声明
//     同一个 AUDIO group，前端 hls.js 切换音轨只重拉音频分片，视频播放位置
//     不受影响；HLS 分片天然可寻址，拖进度条对任意音轨都正常。
//   - 渐进式转码：master.m3u8 只依赖音轨数量即可立即写出，ffmpeg 边转边追加
//     分片（hls_playlist_type=event），请求撞上未产出的分片时由路由层轮询等待。
//   - 视频编码优先 h264 直拷贝，其次 VAAPI 硬编（Tier1 硬解硬编 / Tier2 软解
//     硬编），最后回退 libx264 软件编码，保证任何机器都能出片。
package hls

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"ktvhome/internal/binpath"
	"ktvhome/internal/db"
	"ktvhome/internal/logger"
)

// SegTime 分片时长（秒），只是建议值，ffmpeg 会在最近的关键帧处切割。
const SegTime = 6

// CompleteMarker 转码完成标记文件名。
const CompleteMarker = ".complete"

var (
	safeVideoCodecs = map[string]bool{"h264": true}
	safeAudioCodecs = map[string]bool{"aac": true}
	digitDirRe      = regexp.MustCompile(`^\d+$`)
)

// BuildFailedError 表示转码任务失败（对应 JS 的 code=BUILD_FAILED）。
type BuildFailedError struct {
	Cause error
}

func (e *BuildFailedError) Error() string {
	return "转码失败: " + e.Cause.Error()
}

// WaitTimeoutError 表示等待分片生成超时（对应 JS 的 code=TIMEOUT）。
type WaitTimeoutError struct{}

func (e *WaitTimeoutError) Error() string { return "等待分片生成超时" }

// HLS 持有转码所需配置与运行时状态。
type HLS struct {
	Dir         string // HLS 缓存根目录
	VAAPIDevice string
	MaxAgeDays  int
	NVENC       string // auto|on|off，NVIDIA NVENC 硬件编码开关（1.2.0）

	vaapiMu    sync.Mutex
	vaapiState *bool // nil=尚未探测，true/false=已探测结果

	nvencMu    sync.Mutex
	nvencState *bool // nil=尚未探测，true/false=已探测结果

	buildingMu sync.Mutex
	building   map[int64]struct{}

	errMu       sync.Mutex
	buildErrors map[int64]error
}

// vaapiDeviceCandidates 是 VAAPI 渲染节点的候选路径：
// 显式配置的路径优先，其次按常见布局探测，覆盖 Intel 集显(iHD/i965)、
// AMD 核显/独显(radeonsi)、常见 N 卡开源驱动布局。
var vaapiDeviceCandidates = []string{"/dev/dri/renderD128", "/dev/dri/renderD129", "/dev/dri/card0"}

func New(dir, vaapiDevice string, maxAgeDays int, nvenc string) *HLS {
	if nvenc == "" {
		nvenc = "auto"
	}
	return &HLS{
		Dir:         dir,
		VAAPIDevice: vaapiDevice,
		MaxAgeDays:  maxAgeDays,
		NVENC:       nvenc,
		building:    make(map[int64]struct{}),
		buildErrors: make(map[int64]error),
	}
}

// resolveDevice 返回实际可用的 VAAPI 设备节点：
// 优先用显式配置路径，若不存在则遍历候选路径自动探测（1.2.0 增强，
// 解决宿主机渲染节点路径与默认值不一致时核显探测不到的问题）。
func (h *HLS) resolveDevice() string {
	if h.VAAPIDevice != "" {
		if _, err := os.Stat(h.VAAPIDevice); err == nil {
			return h.VAAPIDevice
		}
	}
	for _, cand := range vaapiDeviceCandidates {
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}
	return h.VAAPIDevice
}

func (h *HLS) OutDir(id int64) string {
	return filepath.Join(h.Dir, strconv.FormatInt(id, 10))
}

func (h *HLS) MasterPath(id int64) string {
	return filepath.Join(h.OutDir(id), "master.m3u8")
}

func (h *HLS) completeMarkerPath(id int64) string {
	return filepath.Join(h.OutDir(id), CompleteMarker)
}

// execCommand 便于测试替换的进程执行钩子。
var execCommand = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// probeCodecName 用 ffprobe 探测指定流的编码名；失败返回空串（走更保险的转码路径）。
func probeCodecName(filepath, selector string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	out, err := execCommand(ctx, binpath.Resolve("ffprobe"),
		"-v", "error",
		"-select_streams", selector,
		"-show_entries", "stream=codec_name",
		"-of", "csv=p=0",
		filepath,
	)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(lines[0]))
}

// runFFmpeg 执行 ffmpeg，出错时返回包含退出码与 stderr 尾部信息的 error。
func runFFmpeg(args ...string) error {
	cmd := exec.Command(binpath.Resolve("ffmpeg"), args...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	b, _ := io.ReadAll(stderr)
	if err := cmd.Wait(); err != nil {
		tail := string(b)
		if len(tail) > 4000 {
			tail = tail[len(tail)-4000:]
		}
		return fmt.Errorf("ffmpeg exit %v: %s", err, tail)
	}
	return nil
}

// hlsOutArgs 生成 HLS 输出参数。使用 event 播放列表：ffmpeg 随分片产出
// 持续刷新 .m3u8，播放器可边转边播，全部转完才写入 #EXT-X-ENDLIST。
func hlsOutArgs(segPattern, playlistPath string) []string {
	return []string{
		"-f", "hls",
		"-hls_time", strconv.Itoa(SegTime),
		"-hls_playlist_type", "event",
		"-hls_flags", "independent_segments+temp_file",
		"-hls_segment_filename", segPattern,
		playlistPath,
	}
}

// ---------- VAAPI 硬件加速探测 ----------

// detectVAAPI 三态缓存探测：设备文件存在时再做一次最小成本的真实编码自检，
// 确保后续歌曲能直接复用可靠结论，而不是每首各自试错。
func (h *HLS) detectVAAPI() bool {
	h.vaapiMu.Lock()
	defer h.vaapiMu.Unlock()
	if h.vaapiState != nil {
		return *h.vaapiState
	}

	// 1.2.0 增强：显式配置路径不可用时自动探测常见候选渲染节点。
	device := h.resolveDevice()
	if device != h.VAAPIDevice && h.VAAPIDevice != "" {
		logger.Info("VAAPI", "配置的渲染节点 "+h.VAAPIDevice+" 不存在，已自动探测到可用节点: "+device)
	}
	h.VAAPIDevice = device

	logger.Info("VAAPI", "开始探测核显硬件加速，渲染节点: "+h.VAAPIDevice)

	if h.VAAPIDevice == "" {
		logger.Warn("VAAPI", "未检测到任何 VAAPI 渲染节点（宿主机可能没有核显/独显，或容器未直通 /dev/dri）—— 核显调用: 失败，本次运行全程使用软件编码 (libx264)")
		h.vaapiState = boolPtr(false)
		return false
	}
	if _, err := os.Stat(h.VAAPIDevice); err != nil {
		logger.Warn("VAAPI", "未检测到渲染节点 "+h.VAAPIDevice+"（宿主机可能没有核显，或安装/升级时 devices 段落被自动裁掉）—— 核显调用: 失败，本次运行全程使用软件编码 (libx264)")
		h.vaapiState = boolPtr(false)
		return false
	}
	logger.Info("VAAPI", "渲染节点 "+h.VAAPIDevice+" 存在，开始做最小成本的真实编码自检（1x1帧/1帧 h264_vaapi 编码）")

	// vainfo 驱动摘要尽力而为的诊断，失败不影响判断。
	{
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		out, err := execCommand(ctx, "vainfo")
		cancel()
		if err != nil {
			logger.Warn("VAAPI", "vainfo 诊断执行失败（不影响后续判断）: "+firstLine(err.Error()))
		} else {
			info := string(out)
			driverLine := ""
			for _, l := range strings.Split(info, "\n") {
				if strings.Contains(l, "Driver version") {
					driverLine = strings.TrimSpace(l)
					break
				}
			}
			if driverLine == "" {
				lines := strings.Split(strings.TrimSpace(info), "\n")
				if len(lines) > 0 {
					driverLine = strings.TrimSpace(lines[0])
				}
			}
			logger.Info("VAAPI", "vainfo 驱动信息: "+driverLine)
		}
	}

	t0 := time.Now()
	err := runFFmpeg(
		"-loglevel", "error",
		"-init_hw_device", "vaapi=va:"+h.VAAPIDevice,
		"-filter_hw_device", "va",
		"-f", "lavfi", "-i", "color=black:size=64x64:rate=1",
		"-frames:v", "1",
		"-vf", "format=nv12,hwupload",
		"-c:v", "h264_vaapi",
		"-f", "null", "-",
	)
	if err != nil {
		logger.Warn("VAAPI", fmt.Sprintf("核显调用: 失败，自检未通过（耗时 %dms），原因: %s —— 回退到软件编码 (libx264)", time.Since(t0).Milliseconds(), lastLine(err.Error())))
		h.vaapiState = boolPtr(false)
		return false
	}
	logger.Info("VAAPI", fmt.Sprintf("核显调用: 成功，自检耗时 %dms —— 后续转码将优先使用 h264_vaapi 硬件编解码", time.Since(t0).Milliseconds()))
	h.vaapiState = boolPtr(true)
	return true
}

// Warmup 启动时预热 VAAPI/NVENC 自检，避免第一首歌额外多等探测耗时。
func (h *HLS) Warmup() {
	logger.Info("VAAPI", "服务启动，开始预热核显自检...")
	ok := h.detectVAAPI()
	logger.Info("VAAPI", fmt.Sprintf("预热完成，核显硬件加速当前%s", map[bool]string{true: "可用", false: "不可用"}[ok]))
	if h.NVENC != "off" {
		h.detectNVENC()
	}
}

// ---------- NVENC 硬件加速探测（1.2.0） ----------

// detectNVENC 探测 NVIDIA h264_nvenc 编码器是否可用：
// ffmpeg -encoders 列出 h264_nvenc（Debian 官方 ffmpeg 通常未启用，需自编译
// 或使用第三方带 nvenc 的构建）后再做一次 1 帧真实编码自检，确保可用才启用。
func (h *HLS) detectNVENC() bool {
	h.nvencMu.Lock()
	defer h.nvencMu.Unlock()
	if h.nvencState != nil {
		return *h.nvencState
	}

	if h.NVENC == "off" {
		logger.Info("NVENC", "NVENC 已被 NVENC_ENABLE=off 显式禁用")
		h.nvencState = boolPtr(false)
		return false
	}

	logger.Info("NVENC", "开始探测 NVIDIA h264_nvenc 编码器...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	out, err := execCommand(ctx, binpath.Resolve("ffmpeg"), "-hide_banner", "-encoders")
	cancel()
	if err != nil || !strings.Contains(string(out), "h264_nvenc") {
		logger.Info("NVENC", "当前 ffmpeg 未包含 h264_nvenc 编码器（Debian 官方构建默认不带 NVIDIA 编码器），NVENC 不可用，转码将使用 VAAPI/libx264")
		h.nvencState = boolPtr(false)
		return false
	}

	t0 := time.Now()
	err = runFFmpeg(
		"-loglevel", "error",
		"-f", "lavfi", "-i", "color=black:size=64x64:rate=1",
		"-frames:v", "1",
		"-c:v", "h264_nvenc", "-preset", "p4",
		"-f", "null", "-",
	)
	if err != nil {
		logger.Warn("NVENC", fmt.Sprintf("NVENC 自检未通过（耗时 %dms），原因: %s —— NVENC 不可用", time.Since(t0).Milliseconds(), lastLine(err.Error())))
		h.nvencState = boolPtr(false)
		return false
	}
	logger.Info("NVENC", fmt.Sprintf("NVENC 自检通过（耗时 %dms），后续转码可用 h264_nvenc 硬件编码", time.Since(t0).Milliseconds()))
	h.nvencState = boolPtr(true)
	return true
}

func boolPtr(v bool) *bool { return &v }

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// ---------- 轨道构建 ----------

func (h *HLS) buildVideoRendition(src, dir, songTag string) error {
	common := []string{"-loglevel", "error", "-y", "-i", src, "-map", "0:v:0", "-an"}
	out := hlsOutArgs(filepath.Join(dir, "video_%04d.ts"), filepath.Join(dir, "video.m3u8"))
	codec := probeCodecName(src, "v:0")
	t0 := time.Now()

	logger.Info("TRANSCODE", fmt.Sprintf("%s 视频轨: 源编码=%s", songTag, codecOrUnknown(codec)))

	if safeVideoCodecs[codec] {
		args := append(append([]string{}, common...), append([]string{"-c:v", "copy"}, out...)...)
		if err := runFFmpeg(args...); err == nil {
			logger.Info("TRANSCODE", fmt.Sprintf("%s 视频轨: 直接封装拷贝(copy)完成，耗时 %dms，未使用核显", songTag, time.Since(t0).Milliseconds()))
			return nil
		} else {
			logger.Warn("TRANSCODE", fmt.Sprintf("%s 视频轨: h264 -c copy 仍失败，改为重新编码: %s", songTag, lastLine(err.Error())))
		}
	}

	// 硬编回退链（1.2.0）：VAAPI（Tier1 硬解硬编 / Tier2 软解硬编）→
	// NVENC（NVIDIA，软解硬编）→ libx264 软件编码兜底。
	if h.detectVAAPI() {
		// Tier 1：硬件解码 + 硬件编码
		t1 := time.Now()
		args := []string{
			"-loglevel", "error", "-y",
			"-hwaccel", "vaapi", "-hwaccel_device", h.VAAPIDevice, "-hwaccel_output_format", "vaapi",
			"-i", src, "-map", "0:v:0", "-an",
			"-c:v", "h264_vaapi",
		}
		args = append(args, out...)
		err := runFFmpeg(args...)
		if err == nil {
			logger.Info("TRANSCODE", fmt.Sprintf("%s 视频轨: 核显调用成功(Tier1 硬解+硬编 h264_vaapi)，耗时 %dms", songTag, time.Since(t1).Milliseconds()))
			return nil
		}
		logger.Warn("TRANSCODE", fmt.Sprintf("%s 视频轨: 核显硬解+硬编失败(Tier1)，尝试软解+硬编(Tier2): %s", songTag, lastLine(err.Error())))

		// Tier 2：软件解码 + 硬件编码
		t2 := time.Now()
		args = append(append([]string{}, common...), []string{
			"-vaapi_device", h.VAAPIDevice,
			"-vf", "format=nv12,hwupload",
			"-c:v", "h264_vaapi",
		}...)
		args = append(args, out...)
		err = runFFmpeg(args...)
		if err == nil {
			logger.Info("TRANSCODE", fmt.Sprintf("%s 视频轨: 核显调用成功(Tier2 软解+硬编 h264_vaapi)，耗时 %dms", songTag, time.Since(t2).Milliseconds()))
			return nil
		}
		logger.Warn("TRANSCODE", fmt.Sprintf("%s 视频轨: 核显调用失败(Tier2 软解+硬编)，尝试 NVENC/软件编码: %s", songTag, lastLine(err.Error())))
	} else {
		logger.Info("TRANSCODE", fmt.Sprintf("%s 视频轨: 核显不可用，尝试 NVENC/软件编码", songTag))
	}

	if h.detectNVENC() {
		tN := time.Now()
		args := append(append([]string{}, common...), []string{
			"-c:v", "h264_nvenc", "-preset", "p4",
			"-b:v", "6000k", "-maxrate", "8000k", "-bufsize", "12000k",
			"-pix_fmt", "yuv420p",
		}...)
		args = append(args, out...)
		err := runFFmpeg(args...)
		if err == nil {
			logger.Info("TRANSCODE", fmt.Sprintf("%s 视频轨: NVIDIA NVENC 硬编(h264_nvenc)成功，耗时 %dms", songTag, time.Since(tN).Milliseconds()))
			return nil
		}
		logger.Warn("TRANSCODE", fmt.Sprintf("%s 视频轨: NVENC 硬编失败，回退到纯软件编码(Tier3 libx264): %s", songTag, lastLine(err.Error())))
	}

	// Tier 3：纯软件编码兜底
	t3 := time.Now()
	args := append(append([]string{}, common...), []string{
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "20", "-pix_fmt", "yuv420p",
	}...)
	args = append(args, out...)
	if err := runFFmpeg(args...); err != nil {
		return err
	}
	logger.Info("TRANSCODE", fmt.Sprintf("%s 视频轨: 软件编码(Tier3 libx264)完成，耗时 %dms，未使用核显", songTag, time.Since(t3).Milliseconds()))
	return nil
}

func (h *HLS) buildAudioRendition(src, dir string, track int64, songTag string) error {
	common := []string{"-loglevel", "error", "-y", "-i", src, "-map", fmt.Sprintf("0:a:%d", track), "-vn"}
	out := hlsOutArgs(filepath.Join(dir, fmt.Sprintf("audio%d_%%04d.ts", track)), filepath.Join(dir, fmt.Sprintf("audio%d.m3u8", track)))
	codec := probeCodecName(src, fmt.Sprintf("a:%d", track))
	trackName := "原唱"
	if track == 1 {
		trackName = "伴唱"
	} else if track >= 2 {
		trackName = fmt.Sprintf("音轨%d", track)
	}
	t0 := time.Now()

	logger.Info("TRANSCODE", fmt.Sprintf("%s 音轨%d(%s): 源编码=%s", songTag, track, trackName, codecOrUnknown(codec)))

	if safeAudioCodecs[codec] {
		args := append(append([]string{}, common...), append([]string{"-c:a", "copy"}, out...)...)
		if err := runFFmpeg(args...); err == nil {
			logger.Info("TRANSCODE", fmt.Sprintf("%s 音轨%d(%s): 直接封装拷贝(copy)完成，耗时 %dms", songTag, track, trackName, time.Since(t0).Milliseconds()))
			return nil
		} else {
			logger.Warn("TRANSCODE", fmt.Sprintf("%s 音轨%d(%s): aac -c copy 仍失败，改为重新编码: %s", songTag, track, trackName, lastLine(err.Error())))
		}
	}

	t1 := time.Now()
	args := append(append([]string{}, common...), append([]string{"-c:a", "aac", "-b:a", "192k"}, out...)...)
	if err := runFFmpeg(args...); err != nil {
		return err
	}
	logger.Info("TRANSCODE", fmt.Sprintf("%s 音轨%d(%s): 软件编码(aac)完成，耗时 %dms", songTag, track, trackName, time.Since(t1).Milliseconds()))
	return nil
}

func codecOrUnknown(codec string) string {
	if codec == "" {
		return "未知"
	}
	return codec
}

// writeMasterPlaylist 立即写出 master.m3u8（只依赖音轨数量，不依赖转码进度）。
func (h *HLS) writeMasterPlaylist(dir string, trackCount int64) {
	names := []string{"原唱"}
	if trackCount >= 2 {
		names = []string{"原唱", "伴唱"}
	}
	var sb strings.Builder
	sb.WriteString("#EXTM3U\n#EXT-X-VERSION:6\n")
	for t := int64(0); t < trackCount; t++ {
		name := ""
		if t < int64(len(names)) {
			name = names[t]
		} else {
			name = fmt.Sprintf("音轨%d", t+1)
		}
		def := "NO"
		if t == 0 {
			def = "YES"
		}
		sb.WriteString(fmt.Sprintf("#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"aud\",NAME=\"%s\",DEFAULT=%s,AUTOSELECT=%s,URI=\"audio%d.m3u8\"\n", name, def, def, t))
	}
	sb.WriteString("#EXT-X-STREAM-INF:BANDWIDTH=8000000,AUDIO=\"aud\"\nvideo.m3u8\n")
	_ = os.WriteFile(filepath.Join(dir, "master.m3u8"), []byte(sb.String()), 0o644)
}

// trackCount 从歌曲记录中取音轨数（至少 1）。
func trackCount(song *db.Song) int64 {
	if song.AudioTracks == nil || *song.AudioTracks < 1 {
		return 1
	}
	return *song.AudioTracks
}

// buildHLS 并发构建视频轨与全部音频轨，全部成功后才写 .complete 标记。
func (h *HLS) buildHLS(song *db.Song, dir string) error {
	tc := trackCount(song)
	songTag := fmt.Sprintf(`[歌曲 id=%d "%s"]`, song.ID, titleOrFilename(song))
	t0 := time.Now()

	logger.Info("TRANSCODE", fmt.Sprintf("%s 开始转码，共 %d 条音轨%s", songTag, tc, map[bool]string{true: "（1=原唱, 2=伴唱）", false: ""}[tc >= 2]))

	var wg sync.WaitGroup
	errCh := make(chan error, 1+int(tc))

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := h.buildVideoRendition(song.Filepath, dir, songTag); err != nil {
			errCh <- err
		}
	}()

	for t := int64(0); t < tc; t++ {
		track := t
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := h.buildAudioRendition(song.Filepath, dir, track, songTag); err != nil {
				errCh <- err
			}
		}()
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		if err != nil {
			logger.Error("TRANSCODE", fmt.Sprintf("%s 转码失败，总耗时 %dms，原因: %s", songTag, time.Since(t0).Milliseconds(), lastLine(err.Error())))
			return err
		}
	}

	_ = os.WriteFile(h.completeMarkerPath(song.ID), []byte(strconv.FormatInt(time.Now().UnixMilli(), 10)), 0o644)
	logger.Info("TRANSCODE", fmt.Sprintf("%s 全部轨道转码完成，总耗时 %dms", songTag, time.Since(t0).Milliseconds()))
	return nil
}

func titleOrFilename(song *db.Song) string {
	if song.Title != "" {
		return song.Title
	}
	return song.Filename
}

// ---------- 渐进式生成 ----------

// isFresh 判断是否已有可复用的完整转码缓存：.complete 标记存在且不早于源文件。
func (h *HLS) isFresh(id int64, srcPath string) bool {
	marker := h.completeMarkerPath(id)
	srcStat, err1 := os.Stat(srcPath)
	outStat, err2 := os.Stat(marker)
	if err1 != nil || err2 != nil {
		return false
	}
	return !outStat.ModTime().Before(srcStat.ModTime())
}

func (h *HLS) isBuilding(id int64) bool {
	h.buildingMu.Lock()
	defer h.buildingMu.Unlock()
	_, ok := h.building[id]
	return ok
}

func (h *HLS) markBuilding(id int64) {
	h.buildingMu.Lock()
	h.building[id] = struct{}{}
	h.buildingMu.Unlock()
}

func (h *HLS) unmarkBuilding(id int64) {
	h.buildingMu.Lock()
	delete(h.building, id)
	h.buildingMu.Unlock()
}

func (h *HLS) getBuildError(id int64) (error, bool) {
	h.errMu.Lock()
	defer h.errMu.Unlock()
	e, ok := h.buildErrors[id]
	return e, ok
}

func (h *HLS) setBuildError(id int64, err error) {
	h.errMu.Lock()
	h.buildErrors[id] = err
	h.errMu.Unlock()
}

func (h *HLS) clearBuildError(id int64) {
	h.errMu.Lock()
	delete(h.buildErrors, id)
	h.errMu.Unlock()
}

// Ensure 返回可用的 master.m3u8 路径。若歌曲尚未转码，则立即写出 master、
// 清空旧产物，并把耗时转码丢到后台执行（渐进式，不阻塞请求）。
func (h *HLS) Ensure(song *db.Song) (string, error) {
	id := song.ID
	songTag := fmt.Sprintf(`[歌曲 id=%d "%s"]`, id, titleOrFilename(song))

	if h.isFresh(id, song.Filepath) {
		logger.Info("TRANSCODE", fmt.Sprintf("%s 命中已转码缓存，直接复用，不重新转码", songTag))
		return h.MasterPath(id), nil
	}

	if h.isBuilding(id) {
		logger.Info("TRANSCODE", fmt.Sprintf("%s 已有转码任务在后台进行中，本次请求直接复用该任务", songTag))
		return h.MasterPath(id), nil
	}

	// 检查与标记在同一把锁内完成，避免并发请求重复创建任务。
	h.buildingMu.Lock()
	if _, ok := h.building[id]; ok {
		h.buildingMu.Unlock()
		return h.MasterPath(id), nil
	}
	h.building[id] = struct{}{}
	h.buildingMu.Unlock()

	// 清空旧目录（上一次失败/源文件替换留下的半成品），立即写出 master。
	dir := h.OutDir(id)
	if err := os.RemoveAll(dir); err != nil {
		h.unmarkBuilding(id)
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.unmarkBuilding(id)
		return "", err
	}
	h.writeMasterPlaylist(dir, trackCount(song))
	h.clearBuildError(id)

	go func() {
		defer h.unmarkBuilding(id)
		if err := h.buildHLS(song, dir); err != nil {
			h.setBuildError(id, err)
		}
	}()

	return h.MasterPath(id), nil
}

// WaitForFile 轮询等待某个分片/子播放列表出现；转码失败或超时则返回错误。
func (h *HLS) WaitForFile(filepath string, songID int64) error {
	const (
		timeoutMs  = 60000
		intervalMs = 200
	)
	deadline := time.Now().Add(timeoutMs * time.Millisecond)
	for {
		if _, err := os.Stat(filepath); err == nil {
			return nil
		}
		if e, ok := h.getBuildError(songID); ok {
			return &BuildFailedError{Cause: e}
		}
		if time.Now().After(deadline) {
			return &WaitTimeoutError{}
		}
		time.Sleep(intervalMs * time.Millisecond)
	}
}

// Remove 删除歌曲的 HLS 缓存产物并清理任务状态。
func (h *HLS) Remove(id int64) {
	_ = os.RemoveAll(h.OutDir(id))
	h.unmarkBuilding(id)
	h.clearBuildError(id)
	logger.Info("TRANSCODE", fmt.Sprintf("[歌曲 id=%d] 已清理 HLS 转码产物", id))
}

// ---------- 缓存每日清理 ----------

// CleanupResult 为一次清理的统计。
type CleanupResult struct {
	Scanned int `json:"scanned"`
	Removed int `json:"removed"`
}

// CleanupExpired 清理孤儿缓存与过期缓存：
//   - 孤儿目录：目录名（歌曲 id）在数据库已无对应记录；
//   - 过期缓存：距 .complete 标记超过 MaxAgeDays；无 .complete 的半成品目录
//     以目录 mtime 兜底判断；
//   - 正在后台转码的目录一律跳过。
func (h *HLS) CleanupExpired(getValidSongIds func() []int64) CleanupResult {
	if _, err := os.Stat(h.Dir); err != nil {
		return CleanupResult{}
	}

	entries, err := os.ReadDir(h.Dir)
	if err != nil {
		logger.Error("HLS_CLEAN", "读取 HLS 缓存目录失败，本次清理已跳过: "+err.Error())
		return CleanupResult{}
	}

	var validIds map[int64]bool
	if getValidSongIds != nil {
		validIds = make(map[int64]bool)
		for _, id := range getValidSongIds() {
			validIds[id] = true
		}
	}

	maxAge := time.Duration(h.MaxAgeDays) * 24 * time.Hour
	now := time.Now()
	scanned, removed := 0, 0

	for _, entry := range entries {
		if !entry.IsDir() || !digitDirRe.MatchString(entry.Name()) {
			continue // 只处理「数字目录名=歌曲id」的规范产物
		}
		id, _ := strconv.ParseInt(entry.Name(), 10, 64)
		if h.isBuilding(id) {
			continue
		}
		scanned++

		var reason string
		if validIds != nil && !validIds[id] {
			reason = "对应歌曲已不在曲库中(孤儿缓存)"
		} else {
			marker := h.completeMarkerPath(id)
			st, err := os.Stat(marker)
			if err == nil {
				if now.Sub(st.ModTime()) > maxAge {
					reason = fmt.Sprintf("距上次转码完成已超过 %d 天", h.MaxAgeDays)
				}
			} else {
				// .complete 不存在：可能是半成品残留（上次转码中途异常退出），
				// 用目录自身 mtime 兜底判断是否陈旧。
				if dirStat, err2 := os.Stat(h.OutDir(id)); err2 == nil && now.Sub(dirStat.ModTime()) > maxAge {
					reason = "半成品缓存目录长期未完成转码，判定为陈旧残留"
				}
			}
		}

		if reason != "" {
			if err := os.RemoveAll(h.OutDir(id)); err != nil {
				logger.Error("HLS_CLEAN", fmt.Sprintf("[歌曲 id=%d] 缓存清理失败: %s", id, err.Error()))
				continue
			}
			h.unmarkBuilding(id)
			h.clearBuildError(id)
			removed++
			logger.Info("HLS_CLEAN", fmt.Sprintf("[歌曲 id=%d] 缓存已清理，原因: %s", id, reason))
		}
	}

	logger.Info("HLS_CLEAN", fmt.Sprintf("本次清理完成：共检查 %d 个缓存目录，清理 %d 个", scanned, removed))
	return CleanupResult{Scanned: scanned, Removed: removed}
}

// ScheduleCleanup 注册每日清理：启动 5 分钟后先跑一次（避开初始扫描高峰），
// 之后每 24 小时跑一次。
func (h *HLS) ScheduleCleanup(getValidSongIds func() []int64) {
	time.AfterFunc(5*time.Minute, func() { h.CleanupExpired(getValidSongIds) })
	go func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			h.CleanupExpired(getValidSongIds)
		}
	}()
	logger.Info("HLS_CLEAN", fmt.Sprintf("HLS 缓存每日清理任务已注册，过期阈值 %d 天", h.MaxAgeDays))
}

// EnsureDir 确保 HLS 根目录存在。
func (h *HLS) EnsureDir() {
	if err := os.MkdirAll(h.Dir, 0o755); err != nil {
		logger.Error("HLS", "创建 HLS 缓存目录失败: "+err.Error())
	}
}

// ListOutDirs 返回全部数字目录（供测试/管理使用）。
func (h *HLS) ListOutDirs() []string {
	var out []string
	entries, err := os.ReadDir(h.Dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() && digitDirRe.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}
