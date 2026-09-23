// Package logger 提供统一日志格式：[时间] [标签] 内容
// 与原 Node.js 版 logger.js 保持一致，输出到 stdout/stderr，
// 配合 docker logs 即可查看。
package logger

import (
	"fmt"
	"os"
	"sync"
	"time"
)

var mu sync.Mutex

func ts() string {
	return time.Now().Format("2006-01-02 15:04:05.000")
}

func fmtLine(tag, msg string) string {
	return fmt.Sprintf("[%s] [%s] %s", ts(), tag, msg)
}

// Info 输出普通日志。
func Info(tag, msg string) {
	mu.Lock()
	defer mu.Unlock()
	fmt.Fprintln(os.Stdout, fmtLine(tag, msg))
}

// Warn 输出警告日志。
func Warn(tag, msg string) {
	mu.Lock()
	defer mu.Unlock()
	fmt.Fprintln(os.Stderr, fmtLine(tag, msg))
}

// Error 输出错误日志。
func Error(tag, msg string) {
	mu.Lock()
	defer mu.Unlock()
	fmt.Fprintln(os.Stderr, fmtLine(tag, msg))
}
