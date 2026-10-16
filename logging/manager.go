/**
 * @Author: LiuShuXin
 * @Description: 复用模块日志对象，统一每日轮转、过期清理和资源释放
 * @File: manager.go
 * Software: Goland
 * @Date: 2026/10/10 10:10
 */

package logging

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/cauliflower-beep/easy-go-framework/terminal"
)

var modulePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
var filePattern = regexp.MustCompile(`^([a-z][a-z0-9_]{0,63})_([0-9]{8})\.log$`)

// Manager 管理模块日志和文件；一个日志目录仅供一个服务进程使用。
type Manager struct {
	mu           sync.Mutex
	app          Identity
	settings     Options
	location     *time.Location
	now          func() time.Time
	console      zapcore.WriteSyncer
	colored      bool
	restoreColor func()
	diagnostics  zapcore.WriteSyncer
	modules      map[string]*zap.Logger
	writers      map[string]*dailyWriter
	admin        *zap.Logger
	stop         chan struct{}
	done         chan struct{}
	stopOnce     sync.Once
	closed       bool
	closeErr     error
}

// New 创建日志管理器。调用方必须在业务停止写日志后调用 Close。
// output 和 diagnostics 由调用方持有，管理器只关闭自己打开的文件。
func New(app Identity, settings Options, output, diagnostics io.Writer) (*Manager, error) {
	return newManager(app, settings, output, diagnostics, time.Now)
}

func newManager(app Identity, settings Options, output, diagnostics io.Writer, now func() time.Time) (*Manager, error) {
	// Go 调用方省略新增颜色选项时使用 auto；配置文件中的空值由 Load 拒绝。
	if settings.Color == "" {
		settings.Color = "auto"
	}
	if err := errors.Join(app.Validate(), settings.Validate()); err != nil {
		return nil, fmt.Errorf("invalid logger configuration: %w", err)
	}
	if output == nil || diagnostics == nil {
		return nil, errors.New("logger output and diagnostics must not be nil")
	}
	location, err := time.LoadLocation(settings.TimeZone)
	if err != nil {
		return nil, fmt.Errorf("load log time zone: %w", err)
	}
	if settings.Output != "console" {
		// 启动时固定目录，避免后续工作目录变化影响轮转和清理。
		settings.Directory, err = filepath.Abs(settings.Directory)
		if err != nil {
			return nil, fmt.Errorf("resolve log directory: %w", err)
		}
	}
	m := &Manager{
		app: app, settings: settings, location: location, now: now,
		console: sink(output), diagnostics: sink(diagnostics),
		restoreColor: func() {},
		modules:      make(map[string]*zap.Logger), writers: make(map[string]*dailyWriter),
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	removed := 0
	if m.fileEnabled() {
		if err := os.MkdirAll(settings.Directory, 0700); err != nil {
			return nil, fmt.Errorf("create log directory: %w", err)
		}
		removed, err = m.cleanup(now())
		if err != nil {
			return nil, fmt.Errorf("clean expired logs at startup: %w", err)
		}
	}
	if settings.Output != "file" && app.Environment == "development" && settings.Format != "json" {
		m.colored, m.restoreColor = terminal.Color(output, settings.Color)
	}
	m.admin, err = m.Module("logging")
	if err != nil {
		m.restoreColor()
		return nil, err
	}
	m.admin.Info("logging initialized",
		zap.String("output", settings.Output), zap.String("time_zone", settings.TimeZone),
		zap.Bool("console_color", m.colored),
		zap.Int("retention_days", settings.RetentionDays), zap.Int("removed_files", removed))
	if m.fileEnabled() {
		go m.maintain()
	} else {
		close(m.done)
	}
	return m, nil
}

// Module 在模块初始化时获取日志对象，同名模块复用同一对象。
// 模块名只允许小写字母、数字、下划线，必须以字母开头，最长 64 字符。
func (m *Manager) Module(name string) (*zap.Logger, error) {
	if !modulePattern.MatchString(name) {
		return nil, errors.New("invalid log module: use 1-64 lowercase letters, digits or underscores, starting with a letter")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, os.ErrClosed
	}
	if logger := m.modules[name]; logger != nil {
		return logger, nil
	}
	var cores []zapcore.Core
	if m.settings.Output != "file" {
		cores = append(cores, newCore(m.settings.Level, m.settings.Format, m.app.Environment, m.console, m.location, m.colored))
	}
	if m.fileEnabled() {
		writer, err := newDailyWriter(m.settings.Directory, name, m.location, m.now)
		if err != nil {
			return nil, fmt.Errorf("create logger for module %q: %w", name, err)
		}
		m.writers[name] = writer
		// 文件始终使用 JSON，保证逐行解析；终端格式可以独立选择。
		cores = append(cores, newCore(m.settings.Level, "json", m.app.Environment, writer, m.location, false))
	}
	logger := newLogger(m.app, m.diagnostics, cores...).With(zap.String("module", name))
	m.modules[name] = logger
	return logger, nil
}

func (m *Manager) fileEnabled() bool { return m.settings.Output != "console" }

func (m *Manager) maintain() {
	defer close(m.done)
	for {
		now := m.now().In(m.location)
		// 使用日历计算下一次零点，不把自然日假定为固定 24 小时。
		next := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, m.location)
		timer := time.NewTimer(next.Sub(now))
		select {
		case <-m.stop:
			timer.Stop()
			return
		case <-timer.C:
			removed, err := m.checkpoint(m.now())
			if err != nil {
				// 独立诊断流不依赖文件可写，文件系统故障时也能报告原因。
				fmt.Fprintln(m.diagnostics, "log maintenance failed:", err)
			}
			m.admin.Info("daily log maintenance completed", zap.Int("removed_files", removed), zap.Bool("success", err == nil))
		}
	}
}

func (m *Manager) checkpoint(now time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return 0, os.ErrClosed
	}
	var problems []error
	for name, writer := range m.writers {
		if err := writer.rotate(now); err != nil {
			problems = append(problems, fmt.Errorf("rotate module %s: %w", name, err))
		}
	}
	// 先切换文件再清理，避免 Windows 文件句柄阻止删除旧文件。
	removed, err := m.cleanup(now)
	return removed, errors.Join(append(problems, err)...)
}

// cleanup 调用时持有管理器锁，或处于尚未向外暴露的构造阶段。
func (m *Manager) cleanup(now time.Time) (int, error) {
	local := now.In(m.location)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, m.location)
	cutoff := today.AddDate(0, 0, 1-m.settings.RetentionDays)
	active := make(map[string]bool, len(m.writers))
	for _, writer := range m.writers {
		active[writer.activePath()] = true
	}
	entries, err := os.ReadDir(m.settings.Directory)
	if err != nil {
		return 0, err
	}
	removed := 0
	var problems []error
	for _, entry := range entries {
		// 只处理专用目录内符合命名规则的普通文件，不递归，不跟随链接。
		match := filePattern.FindStringSubmatch(entry.Name())
		if match == nil || !entry.Type().IsRegular() {
			continue
		}
		date, err := time.ParseInLocation("20060102", match[2], m.location)
		path := filepath.Join(m.settings.Directory, entry.Name())
		if err != nil || !date.Before(cutoff) || active[path] {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			problems = append(problems, fmt.Errorf("remove expired log %s: %w", entry.Name(), err))
		} else if err == nil {
			removed++
		}
	}
	return removed, errors.Join(problems...)
}

// Close 停止定时维护，刷新并关闭所有文件；重复调用返回相同结果。
func (m *Manager) Close() error {
	m.stopOnce.Do(func() { close(m.stop) })
	<-m.done
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return m.closeErr
	}
	m.closed = true
	var problems []error
	for name, writer := range m.writers {
		if err := writer.Close(); err != nil {
			problems = append(problems, fmt.Errorf("close module %s: %w", name, err))
		}
	}
	if m.settings.Output != "file" {
		problems = append(problems, m.console.Sync())
	}
	m.restoreColor()
	m.closeErr = errors.Join(problems...)
	return m.closeErr
}
