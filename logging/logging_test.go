/**
 * @Author: LiuShuXin
 * @Description: 验证日志格式、级别过滤、并发输出及真实刷新失败
 * @File: logging_test.go
 * Software: Goland
 * @Date: 2026/10/10 09:21
 */

package logging

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestJSONMetadataAndStacktrace(t *testing.T) {
	var output bytes.Buffer
	logger := testLogger(t, Options{Output: "console", Directory: "logs", TimeZone: "UTC", RetentionDays: 7, Level: "info", Format: "json"}, &output)
	logger.Info("startup", zap.Bool("http_enabled", false), Redacted("token"))
	logger.Error("operation failed", zap.Error(errors.New("sample failure")))
	if err := logger.Sync(); err != nil {
		t.Fatal(err)
	}
	entries := readEntries(t, output.String())
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	first := entries[0]
	for key, want := range map[string]any{
		"app": "test-service", "environment": "development", "level": "info",
		"message": "startup", "http_enabled": false, "token": "[REDACTED]",
	} {
		if first[key] != want {
			t.Errorf("%s = %v, want %v", key, first[key], want)
		}
	}
	stamp, ok := first["time"].(string)
	if !ok {
		t.Fatalf("missing timestamp: %v", first)
	}
	if _, err := time.Parse(time.RFC3339Nano, stamp); err != nil || !strings.HasSuffix(stamp, "Z") {
		t.Fatalf("timestamp = %q, want RFC3339 in UTC", stamp)
	}
	if caller, _ := first["caller"].(string); !strings.Contains(caller, "logging_test.go:") {
		t.Errorf("caller does not identify the log site: %q", caller)
	}
	if _, exists := first["stacktrace"]; exists {
		t.Error("info entry should not include a stacktrace")
	}
	if stack, _ := entries[1]["stacktrace"].(string); stack == "" || entries[1]["error"] != "sample failure" {
		t.Errorf("error entry lacks diagnostics: %v", entries[1])
	}
}

func TestLevelFiltering(t *testing.T) {
	levels := []string{"debug", "info", "warn", "error"}
	for index, level := range levels {
		t.Run(level, func(t *testing.T) {
			var output bytes.Buffer
			logger := testLogger(t, Options{Output: "console", Directory: "logs", TimeZone: "UTC", RetentionDays: 7, Level: level, Format: "json"}, &output)
			logger.Debug("debug")
			logger.Info("info")
			logger.Warn("warn")
			logger.Error("error")
			entries := readEntries(t, output.String())
			if len(entries) != len(levels)-index {
				t.Fatalf("entries = %d, want %d", len(entries), len(levels)-index)
			}
			for offset, entry := range entries {
				if entry["level"] != levels[index+offset] {
					t.Errorf("unexpected level: %v", entry["level"])
				}
			}
		})
	}
}

func TestFormatSelection(t *testing.T) {
	for _, tc := range []struct {
		environment string
		format      string
		wantJSON    bool
	}{
		{"development", "auto", false}, {"test", "auto", false},
		{"production", "auto", true}, {"development", "json", true},
		{"production", "console", false},
	} {
		t.Run(tc.environment+"/"+tc.format, func(t *testing.T) {
			var output bytes.Buffer
			logger, err := newOutputLogger(Identity{Name: "test-service", Environment: tc.environment}, Options{Output: "console", Directory: "logs", TimeZone: "UTC", RetentionDays: 7, Level: "info", Format: tc.format}, &output, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			logger.Info("format test")
			if json.Valid(bytes.TrimSpace(output.Bytes())) != tc.wantJSON {
				t.Fatalf("unexpected format: %s", output.String())
			}
			if !tc.wantJSON && !strings.Contains(output.String(), "INFO") {
				t.Errorf("console entry lacks readable level: %s", output.String())
			}
		})
	}
}

func TestConcurrentChildLoggers(t *testing.T) {
	var output bytes.Buffer
	logger := testLogger(t, Options{Output: "console", Directory: "logs", TimeZone: "UTC", RetentionDays: 7, Level: "info", Format: "json"}, &output)
	var workers sync.WaitGroup
	for worker := range 12 {
		workers.Go(func() {
			child := logger.With(zap.Int("worker", worker))
			for index := range 40 {
				child.Info("concurrent event", zap.Int("index", index))
			}
		})
	}
	workers.Wait()
	entries := readEntries(t, output.String())
	seen := make(map[[2]int]bool)
	for _, entry := range entries {
		key := [2]int{int(entry["worker"].(float64)), int(entry["index"].(float64))}
		if seen[key] {
			t.Errorf("duplicated event: %v", key)
		}
		seen[key] = true
	}
	if len(entries) != 480 || len(seen) != 480 {
		t.Fatalf("events lost or interleaved: entries=%d unique=%d", len(entries), len(seen))
	}
}

func TestJSONEscapesUntrustedFields(t *testing.T) {
	var output bytes.Buffer
	logger := testLogger(t, Options{Output: "console", Directory: "logs", TimeZone: "UTC", RetentionDays: 7, Level: "info", Format: "json"}, &output)
	value := "line one\n{\"level\":\"error\"}"
	logger.Info("safe encoding", zap.String("value", value))
	entries := readEntries(t, output.String())
	if len(entries) != 1 || entries[0]["value"] != value {
		t.Fatalf("value injected an extra entry: %s", output.String())
	}
}

func TestSyncAndWriteErrors(t *testing.T) {
	t.Run("sync error preserved", func(t *testing.T) {
		failure := errors.New("flush unavailable")
		writer := &syncWriter{failure: failure}
		logger := testLogger(t, Options{Output: "console", Directory: "logs", TimeZone: "UTC", RetentionDays: 7, Level: "info", Format: "json"}, writer)
		logger.Info("before flush")
		if err := logger.Sync(); !errors.Is(err, failure) || writer.syncs != 1 {
			t.Fatalf("Sync() = %v, calls=%d", err, writer.syncs)
		}
	})
	t.Run("write failure reported", func(t *testing.T) {
		var diagnostics bytes.Buffer
		logger, err := newOutputLogger(Identity{Name: "test-service", Environment: "test"}, Options{Output: "console", Directory: "logs", TimeZone: "UTC", RetentionDays: 7, Level: "info", Format: "json"}, failingWriter{}, &diagnostics)
		if err != nil {
			t.Fatal(err)
		}
		logger.Info("unwritable event")
		if !strings.Contains(diagnostics.String(), "sink unavailable") {
			t.Fatalf("write failure lost: %s", diagnostics.String())
		}
	})
}

func TestInvalidConstruction(t *testing.T) {
	validApp := Identity{Name: "test-service", Environment: "test"}
	validLog := Options{Output: "console", Directory: "logs", TimeZone: "UTC", RetentionDays: 7, Level: "info", Format: "json"}
	for _, tc := range []struct {
		name        string
		app         Identity
		settings    Options
		output      io.Writer
		diagnostics io.Writer
	}{
		{"invalid app", Identity{}, validLog, io.Discard, io.Discard},
		{"invalid level", validApp, Options{Output: "console", Directory: "logs", TimeZone: "UTC", RetentionDays: 7, Level: "fatal", Format: "json"}, io.Discard, io.Discard},
		{"invalid format", validApp, Options{Output: "console", Directory: "logs", TimeZone: "UTC", RetentionDays: 7, Level: "info", Format: "xml"}, io.Discard, io.Discard},
		{"nil output", validApp, validLog, nil, io.Discard},
		{"nil diagnostics", validApp, validLog, io.Discard, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logger, err := New(tc.app, tc.settings, tc.output, tc.diagnostics)
			if err == nil || logger != nil {
				t.Fatalf("New() = %v, %v, want nil logger and error", logger, err)
			}
		})
	}
}

func testLogger(t *testing.T, settings Options, output io.Writer) *zap.Logger {
	t.Helper()
	logger, err := newOutputLogger(Identity{Name: "test-service", Environment: "development"}, settings, output, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	return logger
}

func readEntries(t *testing.T, output string) []map[string]any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(output))
	var entries []map[string]any
	for {
		var entry map[string]any
		if err := decoder.Decode(&entry); err != nil {
			if errors.Is(err, io.EOF) {
				return entries
			}
			t.Fatalf("invalid JSON log: %v", err)
		}
		entries = append(entries, entry)
	}
}

type syncWriter struct {
	bytes.Buffer
	failure error
	syncs   int
}

func (w *syncWriter) Sync() error { w.syncs++; return w.failure }

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("sink unavailable") }

// newOutputLogger 隔离文件生命周期，仅验证日志编码与输出行为。
func newOutputLogger(app Identity, settings Options, output, diagnostics io.Writer) (*zap.Logger, error) {
	return newLogger(app, diagnostics, newCore(settings.Level, settings.Format, app.Environment, output, time.UTC, false)), nil
}
