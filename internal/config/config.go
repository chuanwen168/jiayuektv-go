// Package config 负责读取环境变量配置，与原 Node.js 版保持完全一致的默认值，
// 并扩展 1.2.0 版新增配置项（预置管理员密码、网盘曲库、歌手头像目录、NVENC）。
package config

import (
	"os"
	"path/filepath"
	"strconv"
)

type Config struct {
	Port               string // 监听端口，默认 8080
	DataDir            string // 数据目录（数据库/封面），默认 /data
	MVDir              string // 本地曲库根目录，默认 /mv（下辖多个曲库子目录）
	MVNetDir           string // 网盘曲库根目录，默认 /mv-net（下辖多个网盘曲库子目录）
	HLSDir             string // HLS 转码缓存目录，默认 {DataDir}/hls
	VAAPIDevice        string // VAAPI 渲染节点路径，默认 /dev/dri/renderD128（可空则自动探测）
	HLSCacheMaxAgeDays int    // HLS 缓存过期天数，默认 3
	AdminPassword      string // 1.2.0：预置管理员密码（仅首次未设密码时生效），默认空
	SingerDir          string // 1.2.0：歌手头像目录，默认 /singer（图片名=歌手名）
	NVENCEnabled       string // 1.2.0：NVIDIA NVENC 硬件编码开关，auto|on|off，默认 auto
}

func Load() *Config {
	return &Config{
		Port:               getEnv("PORT", "8080"),
		DataDir:            getEnv("DATA_DIR", "/data"),
		MVDir:              getEnv("MV_DIR", "/mv"),
		MVNetDir:           getEnv("MV_NET_DIR", "/mv-net"),
		HLSDir:             "",
		VAAPIDevice:        getEnv("VAAPI_DEVICE", "/dev/dri/renderD128"),
		HLSCacheMaxAgeDays: getEnvInt("HLS_CACHE_MAX_AGE_DAYS", 3),
		AdminPassword:      os.Getenv("ADMIN_PASSWORD"),
		SingerDir:          getEnv("SINGER_DIR", "/singer"),
		NVENCEnabled:       getEnv("NVENC_ENABLE", "auto"),
	}
}

// Resolve 补齐派生配置：HLS 目录默认挂在数据目录之下。
func (c *Config) Resolve() {
	if c.HLSDir == "" {
		c.HLSDir = filepath.Join(c.DataDir, "hls")
	}
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
