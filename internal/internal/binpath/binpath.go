// Package binpath 负责解析 ffmpeg/ffprobe 等外部可执行文件的路径。
//
// Windows 上 Go 的 exec 出于安全考虑不会从当前工作目录查找命令名
// （"cannot run executable found relative to current directory"），
// 而常见用法是把 ffmpeg.exe 直接放在项目/工作目录里且不在 PATH。
// 本包统一按以下优先级解析，保证两套环境都能工作：
//
//	1. 环境变量 <NAME>_DIR（目录，如 FFMPEG_DIR）或 <NAME>_PATH（完整路径）
//	2. 系统 PATH（exec.LookPath）
//	3. 当前工作目录（兼容"ffmpeg.exe 放在项目目录"的用法）
//	4. 原样返回，交由系统解析（最后兜底，可能失败）
package binpath

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Resolve 按优先级解析可执行文件路径。
func Resolve(name string) string {
	key := strings.ToUpper(name)

	// 1. 环境变量：FFMPEG_DIR=目录 / FFMPEG_PATH=完整路径（Windows 可省略 .exe 后缀）
	if dir := os.Getenv(key + "_DIR"); dir != "" {
		if p := candidate(dir, name); p != "" {
			return p
		}
	}
	if p := os.Getenv(key + "_PATH"); p != "" {
		if fileExists(p) {
			return p
		}
		if !isExecSuffixed(p) && fileExists(p+exeSuffix()) {
			return p + exeSuffix()
		}
	}

	// 2. 系统 PATH
	if p, err := exec.LookPath(name); err == nil {
		return p
	}

	// 3. 当前工作目录
	if wd, err := os.Getwd(); err == nil {
		if p := candidate(wd, name); p != "" {
			return p
		}
	}

	// 4. 原样返回，交给系统解析
	return name
}

// candidate 尝试 dir 下的可执行文件（带/不带 .exe）。
func candidate(dir, name string) string {
	if !isExecSuffixed(name) {
		if p := filepath.Join(dir, name+exeSuffix()); fileExists(p) {
			return p
		}
	}
	p := filepath.Join(dir, name)
	if fileExists(p) {
		return p
	}
	return ""
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func isExecSuffixed(p string) bool {
	return strings.HasSuffix(strings.ToLower(p), ".exe")
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}
