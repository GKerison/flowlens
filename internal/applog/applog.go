// Package applog 提供全局日志能力：分级、落文件、按天滚动。
//
// 为什么不用标准 log 或 logrus：
//   - 标准 log 无法分级、无法滚动；
//   - 内核库（go-mitmproxy）自带 logrus 输出，我们用独立实现避免被其配置污染。
//
// 安全约束（务必遵守）：日志中禁止出现 Cookie、Authorization、请求/响应体等可能含
// 用户凭证与隐私的内容。需要定位问题时只记录域名、方法、状态码。
package applog

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Level 是日志级别。
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO "
	case LevelWarn:
		return "WARN "
	default:
		return "ERROR"
	}
}

var (
	mu       sync.Mutex
	file     *os.File
	logDir   string
	fileDate string // 当前日志文件对应的日期，用于跨天滚动
	minLevel = LevelInfo
	// 保留天数：过期日志在初始化时清理
	retainDays = 7
)

// Init 初始化日志：创建目录、清理过期文件、打开当天日志。
func Init(dir string) error {
	mu.Lock()
	defer mu.Unlock()

	logDir = filepath.Join(dir, "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return fmt.Errorf("创建日志目录失败: %w", err)
	}
	cleanupOldLocked()
	return rotateLocked(time.Now())
}

// SetLevel 设置最低输出级别。
func SetLevel(l Level) {
	mu.Lock()
	defer mu.Unlock()
	minLevel = l
}

// Dir 返回日志目录，供界面"打开日志位置"使用。
func Dir() string {
	mu.Lock()
	defer mu.Unlock()
	return logDir
}

// Debugf 输出调试日志，仅在级别允许时写盘。
func Debugf(format string, args ...any) { output(LevelDebug, format, args...) }

// Infof 输出常规日志（启停、安装证书等关键动作）。
func Infof(format string, args ...any) { output(LevelInfo, format, args...) }

// Warnf 输出告警日志（降级、重试、可继续的异常）。
func Warnf(format string, args ...any) { output(LevelWarn, format, args...) }

// Errorf 输出错误日志（失败但进程可继续）。
func Errorf(format string, args ...any) { output(LevelError, format, args...) }

// Close 关闭日志文件，进程退出前调用。
func Close() {
	mu.Lock()
	defer mu.Unlock()
	if file != nil {
		_ = file.Close()
		file = nil
	}
}

// output 是唯一的写入口，内部串行化，保证多 goroutine 下不交错。
func output(l Level, format string, args ...any) {
	mu.Lock()
	defer mu.Unlock()

	if l < minLevel {
		return
	}
	now := time.Now()
	// 跨天时切换文件；rotateLocked 失败则退回 stderr，绝不因日志问题影响主流程
	if file == nil || now.Format("20060102") != fileDate {
		if err := rotateLocked(now); err != nil {
			fmt.Fprintf(os.Stderr, "%s [%s] 日志轮转失败: %v\n", now.Format(time.RFC3339), l, err)
		}
	}

	line := fmt.Sprintf("%s [%s] %s\n", now.Format("2006-01-02 15:04:05.000"), l, fmt.Sprintf(format, args...))
	if file != nil {
		_, _ = file.WriteString(line)
	}
	// 开发期同时输出到 stderr，wails3 dev 时可直接在终端看到
	if os.Getenv("FLOWLENS_LOG_STDERR") != "0" {
		_, _ = os.Stderr.WriteString(line)
	}
}

// rotateLocked 打开（或创建）指定日期的日志文件，调用方必须持有 mu。
func rotateLocked(t time.Time) error {
	if file != nil {
		_ = file.Close()
		file = nil
	}
	date := t.Format("20060102")
	path := filepath.Join(logDir, "flowlens-"+date+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	file = f
	fileDate = date
	return nil
}

// cleanupOldLocked 删除超过保留期的日志，调用方必须持有 mu。
func cleanupOldLocked() {
	entries, err := os.ReadDir(logDir)
	if err != nil {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -retainDays)
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".log" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(logDir, e.Name()))
		}
	}
}
