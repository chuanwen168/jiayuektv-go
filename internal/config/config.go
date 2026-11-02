// Package config 负责读取环境变量配置，与原 Node.js 版保持完全一致的默认值，
// 并扩展 1.2.0 版新增配置项（预置管理员密码、网盘曲库、歌手头像目录、NVENC）。
package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

type Config struct {
	Port               string   // 监听端口，默认 8080
	DataDir            string   // 数据目录（数据库/封面），默认 /data
	MVDir              string   // 主本地曲库根目录（=MVDirs[0]，兼容旧引用）
	MVDirs             []string // 1.0.4：本地曲库根目录列表（MV_DIR 冒号分隔）
	MVNetDir           string   // 网盘曲库根目录，默认 /mv-net（下辖多个网盘曲库子目录）
	HLSDir             string   // HLS 转码缓存目录，默认 {DataDir}/hls
	VAAPIDevice        string   // VAAPI 渲染节点路径，默认 /dev/dri/renderD128（可空则自动探测）
	HLSCacheMaxAgeDays int      // HLS 缓存过期天数，默认 3
	AdminPassword      string   // 1.2.0：预置管理员密码（仅首次未设密码时生效），默认空
	SingerDir          string   // 主歌手头像目录（=SingerDirs[0]，兼容旧引用）
	SingerDirs         []string // 1.0.4：歌手头像目录列表（SINGER_DIR 冒号分隔）
	NVENCEnabled       string   // 1.2.0：NVIDIA NVENC 硬件编码开关，auto|on|off，默认 auto
	TranscodeConcurrency int    // 1.0.4：同时最多允许的 ffmpeg 转码任务数（HLS 转码 + 直连转码共用），按 CPU 核心数自动计算
}

func Load() *Config {
	mvDirs := splitColon(getEnv("MV_DIR", "/mv"))
	singerDirs := splitColon(getEnv("SINGER_DIR", "/singer"))
	return &Config{
		Port:               getEnv("PORT", "8080"),
		DataDir:            getEnv("DATA_DIR", "/data"),
		MVDir:              mvDirs[0],
		MVDirs:             mvDirs,
		MVNetDir:           getEnv("MV_NET_DIR", "/mv-net"),
		HLSDir:             "",
		VAAPIDevice:        getEnv("VAAPI_DEVICE", "/dev/dri/renderD128"),
		HLSCacheMaxAgeDays: getEnvInt("HLS_CACHE_MAX_AGE_DAYS", 3),
		AdminPassword:      os.Getenv("ADMIN_PASSWORD"),
		SingerDir:          singerDirs[0],
		SingerDirs:         singerDirs,
		NVENCEnabled:       getEnv("NVENC_ENABLE", "auto"),
		TranscodeConcurrency: 0, // 始终按 CPU 核心数自动计算（1.0.4，不再支持环境变量覆盖）
	}
}

// splitColon 按冒号拆分路径列表并过滤空项（1.0.4：MV_DIR/SINGER_DIR 支持多目录）。
// Windows 盘符路径（如 D:\mv）整体视为单个目录，不按冒号拆分。
func splitColon(s string) []string {
	if len(s) > 1 && s[1] == ':' &&
		((s[0] >= 'A' && s[0] <= 'Z') || (s[0] >= 'a' && s[0] <= 'z')) {
		return []string{s}
	}
	var out []string
	for _, p := range strings.Split(s, ":") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return []string{s}
	}
	return out
}

// Resolve 补齐派生配置：HLS 目录默认挂在数据目录之下。
func (c *Config) Resolve() {
	if c.HLSDir == "" {
		c.HLSDir = filepath.Join(c.DataDir, "hls")
	}
	if c.TranscodeConcurrency < 1 {
		c.TranscodeConcurrency = autoTranscodeConcurrency()
	}
}

// autoTranscodeConcurrency 根据 CPU 核心数自动计算转码并发数：并发=核心数
//（1核=1、2核=2…8核=8），8 核以上封顶 8。容器内 runtime.NumCPU() 返回的是
// 容器被分配到的核数（Go 1.19+ 读取 cgroup 配额），可自动适配 Docker 限制。
func autoTranscodeConcurrency() int {
	n := runtime.NumCPU()
	if n > 8 {
		return 8
	}
	return n
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getEnvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
