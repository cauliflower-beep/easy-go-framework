/**
 * @Author: LiuShuXin
 * @Description: 验证模块复用、自然日轮转、重启追加、过期清理和关闭边界
 * @File: manager_test.go
 * Software: Goland
 * @Date: 2026/10/10 10:10
 */

package logging

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestModuleReuseAndIsolation(t *testing.T) {
	settings := managerSettings(t)
	settings.Output = "both"
	var output bytes.Buffer
	m := testManager(t, settings, &output, newTestClock())
	var workers sync.WaitGroup
	loggers := make([]*zap.Logger, 12)
	for worker := range 12 {
		workers.Go(func() {
			logger, err := m.Module("game")
			if err != nil {
				t.Error(err)
				return
			}
			loggers[worker] = logger
			for index := range 40 {
				logger.Info("event", zap.Int("worker", worker), zap.Int("index", index))
			}
		})
	}
	workers.Wait()
	for _, logger := range loggers {
		if logger != loggers[0] {
			t.Error("same module did not reuse its logger")
		}
	}
	other, err := m.Module("account")
	if err != nil {
		t.Fatal(err)
	}
	other.Info("account event")
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	entries := readLogFile(t, settings.Directory, "game_20261010.log")
	seen := make(map[[2]int]bool)
	for _, entry := range entries {
		if entry["module"] != "game" || entry["app"] != "test-service" {
			t.Fatalf("wrong module context: %v", entry)
		}
		key := [2]int{int(entry["worker"].(float64)), int(entry["index"].(float64))}
		if seen[key] {
			t.Errorf("duplicate event: %v", key)
		}
		seen[key] = true
	}
	if len(entries) != 480 || len(seen) != 480 || len(readLogFile(t, settings.Directory, "account_20261010.log")) != 1 {
		t.Fatal("events lost, interleaved or routed to the wrong file")
	}
	if len(readEntries(t, output.String())) != 482 {
		t.Fatal("console and files did not receive the same events")
	}
}

func TestWriteRotatesAtLocalMidnightAndRestartAppends(t *testing.T) {
	settings := managerSettings(t)
	clock := newTestClock()
	m := testManager(t, settings, io.Discard, clock)
	logger, err := m.Module("game")
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("before midnight")
	// 上海跨日时 UTC 仍是同一天，证明轮转按配置时区执行。
	clock.set(time.Date(2026, 10, 10, 16, 0, 0, 0, time.UTC))
	logger.Info("after midnight")
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if entries := readLogFile(t, settings.Directory, "game_20261010.log"); len(entries) != 1 || entries[0]["message"] != "before midnight" {
		t.Fatalf("wrong previous-day entries: %v", entries)
	}
	m = testManager(t, settings, io.Discard, clock)
	logger, err = m.Module("game")
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("after restart")
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	entries := readLogFile(t, settings.Directory, "game_20261011.log")
	if len(entries) != 2 || entries[0]["message"] != "after midnight" || entries[1]["message"] != "after restart" {
		t.Fatalf("restart did not append: %v", entries)
	}
	if stamp, _ := entries[0]["time"].(string); !strings.HasSuffix(stamp, "+08:00") {
		t.Errorf("timestamp does not use configured zone: %q", stamp)
	}
}

func TestStartupCleanupCalendarBoundaryAndOwnership(t *testing.T) {
	settings := managerSettings(t)
	for _, name := range []string{
		"game_20261003.log", "retired_module_20261002.log", "game_20261004.log",
		"game_20261010.log", "game_20261020.log", "game_20260230.log",
		"notes.log", "OTHER_20261003.log", "game_20261003.log.bak",
	} {
		writeLogFixture(t, settings.Directory, name, "")
	}
	if err := os.Mkdir(filepath.Join(settings.Directory, "folder_20261003.log"), 0700); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	settings.Output = "both"
	m := testManager(t, settings, &output, newTestClock())
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"game_20261003.log", "retired_module_20261002.log"} {
		if _, err := os.Stat(filepath.Join(settings.Directory, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("expired file was not removed: %s, %v", name, err)
		}
	}
	for _, name := range []string{"game_20261004.log", "game_20261010.log", "game_20261020.log", "game_20260230.log", "notes.log", "OTHER_20261003.log", "game_20261003.log.bak", "folder_20261003.log"} {
		if _, err := os.Stat(filepath.Join(settings.Directory, name)); err != nil {
			t.Errorf("unowned or retained entry was removed: %s, %v", name, err)
		}
	}
	if entries := readEntries(t, output.String()); len(entries) != 1 || entries[0]["removed_files"] != float64(2) {
		t.Fatalf("startup cleanup not reported: %v", entries)
	}
}

func TestCheckpointRotatesIdleModulesAndCleans(t *testing.T) {
	settings := managerSettings(t)
	settings.RetentionDays = 1
	clock := newTestClock()
	m := testManager(t, settings, io.Discard, clock)
	logger, err := m.Module("game")
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("old entry")
	clock.set(clock.now().AddDate(0, 0, 1))
	removed, err := m.checkpoint(clock.now())
	if err != nil || removed != 2 {
		t.Fatalf("checkpoint removed=%d err=%v", removed, err)
	}
	logger.Info("new entry")
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if len(readLogFile(t, settings.Directory, "game_20261011.log")) != 1 {
		t.Fatal("idle module was not switched to the new day")
	}
	if _, err := os.Stat(filepath.Join(settings.Directory, "game_20261010.log")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old idle file remains: %v", err)
	}
}

func TestFailedRotationProtectsActiveFileAndRetries(t *testing.T) {
	settings := managerSettings(t)
	settings.RetentionDays = 1
	clock := newTestClock()
	var diagnostics bytes.Buffer
	m, err := newManager(Identity{Name: "test-service", Environment: "test"}, settings, io.Discard, &diagnostics, clock.now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Error(err)
		}
	})
	logger, err := m.Module("game")
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("previous day")
	blocked := filepath.Join(settings.Directory, "game_20261011.log")
	if err := os.Mkdir(blocked, 0700); err != nil {
		t.Fatal(err)
	}
	clock.set(clock.now().AddDate(0, 0, 1))
	if _, err := m.checkpoint(clock.now()); err == nil {
		t.Fatal("blocked rotation was not reported")
	}
	logger.Info("cannot be written")
	if !strings.Contains(diagnostics.String(), "not a regular file") {
		t.Fatalf("write failure missing: %s", diagnostics.String())
	}
	if len(readLogFile(t, settings.Directory, "game_20261010.log")) != 1 {
		t.Fatal("active file was deleted or new-day event was written into yesterday")
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	logger.Info("recovered")
	if len(readLogFile(t, settings.Directory, "game_20261011.log")) != 1 {
		t.Fatal("rotation did not retry after filesystem recovery")
	}
}

func TestModuleValidationAndClose(t *testing.T) {
	settings := managerSettings(t)
	m := testManager(t, settings, io.Discard, newTestClock())
	for _, name := range []string{"", "../game", "a/b", "a\\b", "UPPER", "_game", "game.log", strings.Repeat("a", 65)} {
		if logger, err := m.Module(name); err == nil || logger != nil {
			t.Errorf("invalid module accepted: %q", name)
		}
	}
	logger, err := m.Module("game")
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			if err := m.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	if _, err := m.Module("game"); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed manager accepted module: %v", err)
	}
	if err := logger.Sync(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("file not closed: %v", err)
	}
}

func TestConsoleOnlyDoesNotCreateDirectory(t *testing.T) {
	settings := managerSettings(t)
	settings.Output = "console"
	settings.Directory = filepath.Join(settings.Directory, "unused")
	var output bytes.Buffer
	m := testManager(t, settings, &output, newTestClock())
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(settings.Directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("console-only mode created directory: %v", err)
	}
	if len(readEntries(t, output.String())) != 1 {
		t.Fatal("logging initialization missing")
	}
}

func TestFileInitializationErrors(t *testing.T) {
	t.Run("directory is file", func(t *testing.T) {
		settings := managerSettings(t)
		settings.Directory = writeLogFixture(t, settings.Directory, "blocked", "")
		if m, err := New(Identity{Name: "test-service", Environment: "test"}, settings, io.Discard, io.Discard); err == nil || m != nil {
			t.Fatalf("invalid directory accepted: %v, %v", m, err)
		}
	})
	t.Run("module path is directory", func(t *testing.T) {
		settings := managerSettings(t)
		m := testManager(t, settings, io.Discard, newTestClock())
		if err := os.Mkdir(filepath.Join(settings.Directory, "game_20261010.log"), 0700); err != nil {
			t.Fatal(err)
		}
		if logger, err := m.Module("game"); err == nil || logger != nil {
			t.Fatalf("invalid module file accepted: %v, %v", logger, err)
		}
	})
}

func managerSettings(t *testing.T) Options {
	t.Helper()
	return Options{Level: "info", Format: "json", Output: "file", Directory: t.TempDir(), TimeZone: "Asia/Shanghai", RetentionDays: 7}
}

func testManager(t *testing.T, settings Options, output io.Writer, clock *testClock) *Manager {
	t.Helper()
	m, err := newManager(Identity{Name: "test-service", Environment: "test"}, settings, output, io.Discard, clock.now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Error(err)
		}
	})
	return m
}

func readLogFile(t *testing.T, directory, name string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(directory, name))
	if err != nil {
		t.Fatal(err)
	}
	return readEntries(t, string(data))
}

func writeLogFixture(t *testing.T, directory, name, contents string) string {
	t.Helper()
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

type testClock struct {
	mu    sync.Mutex
	value time.Time
}

func newTestClock() *testClock {
	return &testClock{value: time.Date(2026, 10, 10, 15, 0, 0, 0, time.UTC)}
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.value
}

func (c *testClock) set(value time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.value = value
}
