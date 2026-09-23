// Package scanner 负责 MV 曲库扫描：
// 多曲库来源发现（/mv 本地 + /mv-net 网盘，每个子目录一个来源）、
// 后台启用/禁用、递归收集视频文件、ffprobe 探测音轨数、
// 按「歌手 - 歌名」（歌手1&歌手2 多歌手）解析文件名、渐进式入库、
// 补全旧曲目音轨数、清理源文件已缺失的记录（仅限启用来源管辖范围）。
package scanner

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"ktvhome/internal/binpath"
	"ktvhome/internal/db"
	"ktvhome/internal/logger"
)

// VideoExt 支持的视频格式白名单。
var VideoExt = map[string]bool{
	".mp4": true, ".mkv": true, ".avi": true, ".flv": true,
	".mov": true, ".webm": true, ".mpg": true,
}

// ScanResult 是 /api/scan 的响应体。Error 仅在异常时非空
// （例如所有启用曲库来源都不可访问时置为 "MV_DIR_UNAVAILABLE"）。
type ScanResult struct {
	Total   int    `json:"total"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
	Error   string `json:"error,omitempty"`
}

// Source 描述一个曲库来源（1.2.0 多曲库功能）。
//   - /mv 根目录直接放文件 → 默认来源（Name=""，无前缀），默认启用
//   - /mv/<子目录> 与 /mv-net/<子目录> → 子目录来源（Name=目录名），默认禁用
//   - /mv-net 根目录直接放文件 → 网盘默认来源（Name="net"），默认禁用
//
// filename 入库规则：Name 为空时用相对来源根的路径；否则用 "<Name>/<相对路径>"，
// 保证多个来源之间不会重名冲突。
type Source struct {
	Name    string `json:"name"`
	Label   string `json:"label"`   // 展示名（目录路径）
	Root    string `json:"root"`    // 来源根绝对路径
	Kind    string `json:"kind"`    // local | net
	Enabled bool   `json:"enabled"` // 是否参与扫描
}

// RemoveHLS 用于在清理缺失曲目时同步删除其 HLS 缓存，由 main 注入，
// 避免 scanner 与 hls 包形成循环依赖。
type RemoveHLS func(id int64)

// Scanner 持有扫描所需的环境配置。
type Scanner struct {
	MVDir     string // 本地曲库根，默认 /mv
	MVNetDir  string // 网盘曲库根，默认 /mv-net
	RemoveHLS RemoveHLS
}

// settingKey 返回来源启用状态的 settings 键。
func sourceSettingKey(name string) string {
	return "library_source:" + name
}

// sourceEnabled 读取来源启用状态；默认来源（Name=""）默认启用，其余默认禁用。
func sourceEnabled(name string) bool {
	if name == "" {
		return true
	}
	v, ok, err := db.GetSetting(sourceSettingKey(name))
	if err != nil {
		logger.Error("SCAN", "读取曲库来源状态失败("+name+"): "+err.Error())
		return false
	}
	return ok && v == "1"
}

// EnableSource 设置曲库来源启用状态（后台「曲库来源」管理）。
func EnableSource(name string, enabled bool) error {
	v := "0"
	if enabled {
		v = "1"
	}
	return db.SetSetting(sourceSettingKey(name), v)
}

// DiscoverSources 扫描曲库根目录，列出全部可用来源及其启用状态。
func (s *Scanner) DiscoverSources() []Source {
	var out []Source
	seen := map[string]bool{} // 已占用的来源名，避免 /mv 与 /mv-net 子目录重名冲突

	// 本地根 /mv：根目录文件 → 默认来源；一级子目录 → 子目录来源。
	if add, ok := s.rootSources(s.MVDir, "local", "", true, seen); ok {
		out = append(out, add...)
	}

	// 网盘根 /mv-net：根目录文件 → Name="net" 默认来源；一级子目录 → 子目录来源。
	if s.MVNetDir != "" && s.MVNetDir != s.MVDir {
		if add, ok := s.rootSources(s.MVNetDir, "net", "net", false, seen); ok {
			out = append(out, add...)
		}
	}
	return out
}

// rootSources 枚举一个根目录下的来源。netPrefix 用于网盘默认来源命名
// （避免与本地默认来源 Name="" 冲突），seen 用于子目录重名去重。
func (s *Scanner) rootSources(root, kind, netPrefix string, defaultEnabled bool, seen map[string]bool) ([]Source, bool) {
	entries, err := os.ReadDir(root)
	if err != nil {
		// 根目录本身不可访问：不报错（可能未挂载），交给扫描阶段统一判断。
		return nil, false
	}

	var out []Source
	// 根目录直接有视频文件 → 默认来源（本地 Name="", 网盘 Name=netPrefix）。
	hasRootFile := false
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if VideoExt[strings.ToLower(filepath.Ext(e.Name()))] {
			hasRootFile = true
			break
		}
	}
	if hasRootFile {
		name := netPrefix
		label := root
		out = append(out, Source{Name: name, Label: label, Root: root, Kind: kind, Enabled: sourceEnabled(name)})
		if name != "" {
			seen[name] = true
		}
	}

	// 一级子目录 → 子目录来源。
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		// 网盘子目录与本地子目录重名时，加 "_net" 后缀避免 filename 前缀冲突。
		if kind == "net" {
			if name == netPrefix || seen[name] {
				name = name + "_net"
			}
		}
		sub := filepath.Join(root, e.Name())
		out = append(out, Source{Name: name, Label: e.Name(), Root: sub, Kind: kind, Enabled: sourceEnabled(name)})
		seen[name] = true
	}
	return out, true
}

// ProbeAudioTracks 用 ffprobe 读取音频流数量，失败按单音轨处理。
func ProbeAudioTracks(filepath string) int64 {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	out, err := execCommand(ctx, binpath.Resolve("ffprobe"),
		"-v", "error",
		"-select_streams", "a",
		"-show_entries", "stream=index",
		"-of", "csv=p=0",
		filepath,
	)
	if err != nil {
		// ffprobe 不可用（如 gyan.dev essentials 版 ffmpeg 不含 ffprobe）时，
		// 回退用 ffmpeg -i 解析 stderr 中的音频流数量，避免多音轨被误判为单音轨。
		logger.Warn("SCAN", "ffprobe 不可用("+filepath+"): "+err.Error()+"，尝试 ffmpeg 回退探测")
		return probeWithFFmpeg(filepath)
	}
	count := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	if count > 0 {
		return int64(count)
	}
	return 1
}

// audioStreamRe 匹配 ffmpeg -i 输出中的音频流行（Stream #0:1: Audio: ...）。
var audioStreamRe = regexp.MustCompile(`Stream\s+#\d+:\d+(?:\([^)]*\))?(?:\[[^\]]*\])?:\s*Audio`)

// probeWithFFmpeg 无 ffprobe 时用 ffmpeg -i 探测音轨数：
// ffmpeg 会把媒体流信息打到 stderr，统计 Audio 流行数即可。
func probeWithFFmpeg(filepath string) int64 {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, binpath.Resolve("ffmpeg"), "-hide_banner", "-i", filepath)
	var buf bytes.Buffer
	cmd.Stderr = &buf
	_ = cmd.Run() // 未指定输出必然非零退出，忽略错误，只需 stderr 的流信息

	if n := len(audioStreamRe.FindAllString(buf.String(), -1)); n > 0 {
		return int64(n)
	}
	logger.Warn("SCAN", "ffmpeg 回退探测也未识别到音频流("+filepath+")，按单音轨处理")
	return 1
}

// execCommand 封装 exec.CommandContext 输出捕获（便于测试替换）。
var execCommand = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.Output()
}

// listFilesRecursive 递归收集曲库视频文件（用于子目录来源）：
// os.Stat 会跟随符号链接，断链/目标已删除的文件在此被排除；
// 单个坏目录只跳过不中断整体扫描。
func listFilesRecursive(dir string) []string {
	var results []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		logger.Error("SCAN", "曲库目录读取失败，已跳过("+dir+"): "+err.Error())
		return results
	}
	for _, entry := range entries {
		full := filepath.Join(dir, entry.Name())
		// 跟随符号链接校验目标真实存在性，死链/不可访问跳过。
		info, err := os.Stat(full)
		if err != nil {
			continue
		}
		if info.IsDir() {
			results = append(results, listFilesRecursive(full)...)
		} else if VideoExt[strings.ToLower(filepath.Ext(entry.Name()))] {
			results = append(results, full)
		}
	}
	return results
}

// rootLevelFiles 只收集目录根级（不递归子目录）的视频文件。
// 用于默认根来源（/mv、/mv-net 根直接放文件的用法）：子目录由各自来源
// 负责扫描，默认根不再递归，避免与子目录来源重复入库。
func rootLevelFiles(dir string) []string {
	var results []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		logger.Error("SCAN", "曲库目录读取失败，已跳过("+dir+"): "+err.Error())
		return results
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		full := filepath.Join(dir, entry.Name())
		info, err := os.Stat(full)
		if err != nil {
			continue
		}
		if !info.IsDir() && VideoExt[strings.ToLower(filepath.Ext(entry.Name()))] {
			results = append(results, full)
		}
	}
	return results
}

// DefaultLanguages 语种预设（1.2.0：文件名尾部 "-语种-风格" 解析 + 后台可自定义）。
var DefaultLanguages = []string{"国语", "粤语", "英语", "日语", "韩语", "其他"}

// DefaultGenres 风格预设。文件名里的变体（如"流行歌曲"）会归一化到基础风格。
var DefaultGenres = []string{"流行", "摇滚", "怀旧", "儿歌", "民谣", "舞曲", "合唱", "DJ", "其他"}

// genreAliases 风格变体归一化：文件名里常见的"XX歌曲/XX风"等后缀归一为预设风格。
var genreAliases = map[string]string{
	"流行歌曲": "流行", "流行风": "流行",
	"摇滚歌曲": "摇滚", "摇滚风": "摇滚",
	"怀旧歌曲": "怀旧", "怀旧风": "怀旧",
	"儿歌":   "儿歌",
	"民谣歌曲": "民谣",
	"舞曲":   "舞曲",
	"合唱":   "合唱",
	"DJ":   "DJ",
	"其他":   "其他",
}

// NormalizeGenre 将风格变体（流行歌曲/摇滚风等）归一为预设基础风格；
// 无法识别时原样返回。
func NormalizeGenre(g string) string {
	g = strings.TrimSpace(g)
	if norm, ok := genreAliases[g]; ok {
		return norm
	}
	return g
}

// SplitArtists 把"歌手1&歌手2"等多歌手字符串按 & 拆分为独立歌手列表。
// 空格不参与拆分（英文歌手名含空格，如 "Connie Talbot"）。
func SplitArtists(artist string) []string {
	var out []string
	for _, a := range strings.Split(artist, "&") {
		a = strings.TrimSpace(a)
		if a != "" {
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		out = append(out, "未知歌手")
	}
	return out
}

// ParseFilename 解析文件名（不含路径）为歌手、歌名、语种、风格。
// 支持格式：
//
//	歌手 - 歌名.mp4               （空格-空格分隔，歌手内可含 &）
//	歌手-歌名.mkv                （无空格 - 分隔）
//	歌手-歌名-语种-风格.mp4       （尾部语种/风格，如 郭静-心墙-国语-流行歌曲.mkv）
//	歌手&歌手2 - 歌名.mkv         （多歌手）
//
// 语种/风格仅在尾部段**同时命中**语种集与风格集时才提取，避免误拆歌名中的 "-"。
func ParseFilename(filename string) (artist, title, language, genre string) {
	base := filepath.Base(filename)
	base = strings.TrimSuffix(base, filepath.Ext(base))

	// 第一步：切出歌手与歌名（优先空格-空格，其次 -，最后 _）。
	title = base
	for _, sep := range []string{" - ", "-", "_"} {
		if idx := strings.Index(base, sep); idx >= 0 {
			a := strings.TrimSpace(base[:idx])
			t := strings.TrimSpace(base[idx+len(sep):])
			if a != "" && t != "" {
				artist, title = a, t
				break
			}
		}
	}
	if artist == "" {
		artist = "未知歌手"
	}

	// 第二步：从歌名尾部提取 "-语种-风格"。
	// 支持 歌名-语种-风格（三段）与 语种-风格（两段、无歌名）两种尾部。
	parts := strings.Split(title, "-")
	if len(parts) >= 2 {
		last := strings.TrimSpace(parts[len(parts)-1])
		prev := strings.TrimSpace(parts[len(parts)-2])
		lang, langOK := matchLanguage(prev)
		gen, genOK := matchGenre(last)
		if langOK && genOK {
			language = lang
			genre = gen
			title = strings.TrimSpace(strings.Join(parts[:len(parts)-2], "-"))
		}
	}
	if title == "" {
		title = "未知歌名"
	}
	return artist, title, language, genre
}

func matchLanguage(s string) (string, bool) {
	for _, l := range DefaultLanguages {
		if s == l {
			return l, true
		}
	}
	switch s {
	case "中文", "普通话":
		return "国语", true
	}
	return s, false
}

func matchGenre(s string) (string, bool) {
	if norm, ok := genreAliases[s]; ok {
		return norm, true
	}
	for _, g := range DefaultGenres {
		if s == g {
			return g, true
		}
	}
	return s, false
}

// ScanLibrary 执行一轮完整扫描，返回统计结果。
func (s *Scanner) ScanLibrary() ScanResult {
	sources := s.DiscoverSources()
	enabled := make([]Source, 0, len(sources))
	for _, src := range sources {
		if src.Enabled {
			enabled = append(enabled, src)
		}
	}
	if len(enabled) == 0 {
		logger.Error("SCAN", "没有任何启用的曲库来源，已跳过本次扫描以避免误删曲库")
		return ScanResult{Error: "MV_DIR_UNAVAILABLE"}
	}

	// 安全防护：所有启用来源根都不可访问时直接中止，绝不触发"清理已缺失记录"。
	okRoot := 0
	for _, src := range enabled {
		if _, err := os.Stat(src.Root); err == nil {
			okRoot++
		}
	}
	if okRoot == 0 {
		logger.Error("SCAN", "曲库目录不可访问，已跳过本次扫描以避免误删曲库")
		return ScanResult{Error: "MV_DIR_UNAVAILABLE"}
	}

	// 收集已入库文件名（全量，用于渐进式去重）。
	existingSet := make(map[string]bool)
	rows, err := db.DB.Query("SELECT filename FROM songs")
	if err == nil {
		for rows.Next() {
			var fn string
			if rows.Scan(&fn) == nil {
				existingSet[fn] = true
			}
		}
		rows.Close()
	}

	// 渐进式入库：逐来源逐文件探测、立即入库，扫描过程中的新歌立即可查。
	// 默认根来源只扫根级文件，子目录来源递归；filename 统一用正斜杠
	// （filepath.ToSlash），保证跨平台（Windows 开发 / Linux 部署）一致。
	added := 0
	total := 0
	validSet := make(map[string]bool) // 当前启用来源下有效 filename 集合（用于清理）
	for _, src := range enabled {
		if _, err := os.Stat(src.Root); err != nil {
			logger.Error("SCAN", "曲库来源不可访问，已跳过("+src.Label+"): "+err.Error())
			continue
		}
		var files []string
		if src.Name == "" {
			files = rootLevelFiles(src.Root)
		} else {
			files = listFilesRecursive(src.Root)
		}
		prefix := src.Name
		if prefix != "" {
			prefix += "/"
		}
		for _, f := range files {
			rel, err := filepath.Rel(src.Root, f)
			if err != nil {
				rel = f
			}
			filename := prefix + filepath.ToSlash(rel)
			validSet[filename] = true
			total++
			if existingSet[filename] {
				continue
			}
			artist, title, language, genre := ParseFilename(f)
			tracks := ProbeAudioTracks(f)
			res, err := db.DB.Exec(
				`INSERT INTO songs (title, artist, filename, filepath, audio_tracks, language, genre)
				 VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(filename) DO NOTHING`,
				title, artist, filename, f, tracks, language, genre,
			)
			if err != nil {
				logger.Error("SCAN", "曲库扫描-新增文件入库失败("+filename+"): "+err.Error())
				continue
			}
			if n, _ := res.RowsAffected(); n > 0 {
				added++
				existingSet[filename] = true
			}
		}
	}

	// 兼容旧版本：给 audio_tracks 为空的老曲目补测音轨数。
	s.backfillAudioTracks()

	// 重建歌手拆分关联表：把每首歌 artist 字段按 & 拆分后逐位写入
	// song_artists（含管理员在后台改过的 artist），TV/手机歌手列表独立可见。
	rebuildSongArtists()

	// 清理源文件已不存在的记录（仅清理启用来源管辖范围内的记录，
	// 未启用/已移除来源的记录保留，避免误删未挂载曲库）。
	removed := s.removeMissing(validSet, enabled)

	return ScanResult{Total: total, Added: added, Removed: removed}
}

// RebuildSongArtists 供 main（歌曲编辑后）调用：重建全部歌手拆分关联。
func RebuildSongArtists() {
	rebuildSongArtists()
}

// rebuildSongArtists 全量重建 song_artists：DELETE 后按 songs.artist 的
// & 拆分重插。曲库规模内开销可忽略（单表全量重写，扫描末尾执行一次）。
func rebuildSongArtists() {
	if _, err := db.DB.Exec("DELETE FROM song_artists"); err != nil {
		logger.Error("SCAN", "歌手关联表重建失败(清空): "+err.Error())
		return
	}
	rows, err := db.DB.Query("SELECT id, artist FROM songs WHERE artist IS NOT NULL AND artist != ''")
	if err != nil {
		logger.Error("SCAN", "歌手关联表重建失败(读取): "+err.Error())
		return
	}
	type pair struct {
		id     int64
		artist string
	}
	var all []pair
	for rows.Next() {
		var p pair
		if rows.Scan(&p.id, &p.artist) == nil {
			all = append(all, p)
		}
	}
	rows.Close()

	tx, err := db.DB.Begin()
	if err != nil {
		logger.Error("SCAN", "歌手关联表重建失败(事务): "+err.Error())
		return
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare("INSERT INTO song_artists (song_id, artist) VALUES (?, ?)")
	if err != nil {
		logger.Error("SCAN", "歌手关联表重建失败(准备): "+err.Error())
		return
	}
	defer stmt.Close()
	for _, p := range all {
		for _, a := range SplitArtists(p.artist) {
			if _, err := stmt.Exec(p.id, a); err != nil {
				logger.Error("SCAN", "歌手关联表重建失败(写入): "+err.Error())
				return
			}
		}
	}
	if err := tx.Commit(); err != nil {
		logger.Error("SCAN", "歌手关联表重建失败(提交): "+err.Error())
	}
}

func (s *Scanner) backfillAudioTracks() {
	rows, err := db.DB.Query("SELECT id, filepath FROM songs WHERE audio_tracks IS NULL")
	if err != nil {
		logger.Error("SCAN", "曲库扫描-音轨补全阶段失败: "+err.Error())
		return
	}
	defer rows.Close()

	type pending struct {
		id       int64
		filepath string
	}
	var items []pending
	for rows.Next() {
		var p pending
		if rows.Scan(&p.id, &p.filepath) == nil {
			items = append(items, p)
		}
	}
	for _, p := range items {
		if _, err := db.DB.Exec("UPDATE songs SET audio_tracks = ? WHERE id = ?", ProbeAudioTracks(p.filepath), p.id); err != nil {
			logger.Error("SCAN", "曲库扫描-音轨补全失败(id="+itoa(p.id)+"): "+err.Error())
		}
	}
}

// removeMissing 删除数据库中源文件已不存在的歌曲记录及其关联数据。
//   - filename 先统一转为正斜杠（filepath.ToSlash）参与判断，兼容旧数据里
//     可能的反斜杠（Windows 开发环境早期版本产生）；
//   - 规范化后重复的记录（同一文件在默认根递归时期与子目录来源各入一次库）
//     只保留一条：优先保留本来就是正斜杠规范化的记录，其次保留较小 id；
//   - 只处理属于启用来源管辖范围的记录：有效集合之外的 filename，若前缀匹配
//     任一启用来源则判定为已缺失并清理；不匹配任何启用来源的记录（属于
//     未启用/已移除来源）一律保留。
func (s *Scanner) removeMissing(validSet map[string]bool, sources []Source) int {
	// 来源前缀规则：Name="" 的默认来源用空串表示"无前缀根文件"；
	// rootPath 用于判断来源目录当前是否可访问（不可访问时保留记录防误删）。
	type prefixRule struct {
		prefix   string
		root     bool   // 默认根（无前缀），只匹配不含 "/" 的根级 filename
		rootPath string // 来源根路径
	}
	var rules []prefixRule
	for _, src := range sources {
		if src.Name == "" {
			rules = append(rules, prefixRule{prefix: "", root: true, rootPath: src.Root})
		} else {
			rules = append(rules, prefixRule{prefix: src.Name + "/", rootPath: src.Root})
		}
	}
	belongsToEnabled := func(filename string) *prefixRule {
		for i := range rules {
			r := &rules[i]
			if r.root {
				if !strings.Contains(filename, "/") {
					return r
				}
				continue
			}
			if strings.HasPrefix(filename, r.prefix) {
				return r
			}
		}
		return nil
	}

	rows, err := db.DB.Query("SELECT id, filename FROM songs")
	if err != nil {
		logger.Error("SCAN", "曲库扫描-清理缺失文件阶段失败: "+err.Error())
		return 0
	}
	defer rows.Close()

	type songRef struct {
		id       int64
		filename string
	}
	var all []songRef
	for rows.Next() {
		var r songRef
		if rows.Scan(&r.id, &r.filename) == nil {
			all = append(all, r)
		}
	}

	removed := 0
	normSeen := make(map[string]int64) // 规范化 filename -> 保留的记录 id
	normFilenames := make(map[int64]string)
	del := func(id int64) {
		if err := deleteSongAndRefs(id); err != nil {
			logger.Error("SCAN", "曲库扫描-删除已缺失曲目失败(id="+itoa(id)+"): "+err.Error())
			return
		}
		if s.RemoveHLS != nil {
			s.RemoveHLS(id)
		}
		removed++
	}
	for _, r := range all {
		key := filepath.ToSlash(r.filename)
		// 规范化后重复：优先保留正斜杠记录，其次保留较小 id。
		if prevID, ok := normSeen[key]; ok {
			prevF := normFilenames[prevID]
			keep := prevID
			if !strings.Contains(r.filename, "\\") && strings.Contains(prevF, "\\") {
				keep = r.id
			} else if r.id < prevID && strings.Contains(r.filename, "\\") == strings.Contains(prevF, "\\") {
				keep = r.id
			}
			delID := prevID
			if keep == prevID {
				delID = r.id
			}
			normSeen[key] = keep
			normFilenames[keep] = "" // 保留的是规范化记录
			del(delID)
			continue
		}
		normSeen[key] = r.id
		normFilenames[r.id] = r.filename
		if validSet[key] {
			continue
		}
		rule := belongsToEnabled(key)
		if rule == nil {
			// 不属于任何已知来源（来源目录已被移除/未发现）：保留，防误删。
			continue
		}
		if _, err := os.Stat(rule.rootPath); err != nil {
			// 来源目录当前不可访问（未挂载/暂离线）：保留，防误删。
			logger.Warn("SCAN", fmt.Sprintf("来源目录不可访问(%s)，保留曲目记录: %s", rule.rootPath, key))
			continue
		}
		logger.Info("SCAN", "清理已删除文件对应的曲目: "+key)
		del(r.id)
	}
	return removed
}

// deleteSongAndRefs 在单个事务中删除歌曲及其所有关联引用
// （queue 对 songs 有真实外键，done 状态的队列记录会挡住删除，必须一并清理）。
func deleteSongAndRefs(id int64) error {
	tx, err := db.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, q := range []string{
		"DELETE FROM queue WHERE song_id = ?",
		"DELETE FROM history WHERE song_id = ?",
		"DELETE FROM favorites WHERE song_id = ?",
		"DELETE FROM song_artists WHERE song_id = ?",
		"DELETE FROM songs WHERE id = ?",
	} {
		if _, err := tx.Exec(q, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// itoa 极简 int64 转字符串。
func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}
