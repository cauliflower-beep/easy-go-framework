/**
 * @Author: LiuShuXin
 * @Description: 按指定时区的自然日追加日志并管理文件生命周期
 * @File: daily_writer.go
 * Software: Goland
 * @Date: 2026/10/10 10:10
 */

package logging

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// dailyWriter 只管理当前文件；保留策略由 Manager 统一执行。
type dailyWriter struct {
	mu        sync.Mutex
	directory string
	module    string
	location  *time.Location
	now       func() time.Time
	file      *os.File
	date      string
	closed    bool
}

func newDailyWriter(directory, module string, location *time.Location, now func() time.Time) (*dailyWriter, error) {
	w := &dailyWriter{directory: directory, module: module, location: location, now: now}
	if err := w.rotate(now()); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *dailyWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, os.ErrClosed
	}
	date := w.now().In(w.location).Format("20060102")
	// 写入时检查日期，保证定时任务延迟时也不会写进昨天的文件。
	rotationErr := w.rotateLocked(date)
	if w.date != date {
		return 0, rotationErr
	}
	n, writeErr := w.file.Write(data)
	return n, errors.Join(rotationErr, writeErr)
}

func (w *dailyWriter) rotate(now time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return os.ErrClosed
	}
	return w.rotateLocked(now.In(w.location).Format("20060102"))
}

func (w *dailyWriter) rotateLocked(date string) error {
	if date == w.date {
		return nil
	}
	path := filepath.Join(w.directory, w.module+"_"+date+".log")
	// 重启同日追加；拒绝目录和符号链接，避免意外覆盖其他资源。
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("log path is not a regular file: %s", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect log file: %w", err)
	}
	next, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	// 先打开新文件，失败时仍保留旧文件，下一次写入可以重试。
	var closeErr error
	if w.file != nil {
		closeErr = errors.Join(w.file.Sync(), w.file.Close())
	}
	w.file, w.date = next, date
	return closeErr
}

func (w *dailyWriter) activePath() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return filepath.Join(w.directory, w.module+"_"+w.date+".log")
}

func (w *dailyWriter) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return os.ErrClosed
	}
	return w.file.Sync()
}

func (w *dailyWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	return errors.Join(w.file.Sync(), w.file.Close())
}
