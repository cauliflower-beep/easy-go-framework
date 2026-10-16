/**
 * @Author: LiuShuXin
 * @Description: 验证四级控制台配色、字段复位与生产及文件输出隔离
 * @File: color_test.go
 * Software: Goland
 * @Date: 2026/10/10 16:00
 */

package logging

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestColoredConsoleAndJSONFileIsolation(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	settings := managerSettings(t)
	settings.Output, settings.Format, settings.Color, settings.Level = "both", "console", "always", "debug"
	var output bytes.Buffer
	m := colorTestManager(t, settings, &output)
	logger, err := m.Module("game")
	if err != nil {
		t.Fatal(err)
	}
	logger = logger.With(zap.String("request_id", "sample-id"))
	logger.Debug("debug event")
	logger.Info("info event")
	logger.Warn("warn event")
	logger.Error("error event")
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, level := range []string{"\x1b[35mDEBUG\x1b[0m", "\x1b[36mINFO\x1b[0m", "\x1b[33mWARN\x1b[0m", "\x1b[31mERROR\x1b[0m"} {
		if !strings.Contains(text, level) {
			t.Fatalf("missing colored level %q: %q", level, text)
		}
	}
	// 每个有颜色的级别只有开始与复位两个序列，正文和堆栈不再着色。
	if strings.Count(text, "\x1b[") != 10 {
		t.Fatalf("colors leaked beyond the five level labels: %q", text)
	}
	data, err := os.ReadFile(filepath.Join(settings.Directory, "game_20261010.log"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("\x1b")) || bytes.Contains(data, []byte("\\u001b")) {
		t.Fatalf("file contains ANSI: %s", data)
	}
	entries := readEntries(t, string(data))
	if len(entries) != 4 {
		t.Fatalf("file lost events: %v", entries)
	}
	for i, level := range []string{"debug", "info", "warn", "error"} {
		if entries[i]["level"] != level || entries[i]["module"] != "game" || entries[i]["request_id"] != "sample-id" {
			t.Fatalf("plain file metadata changed: %v", entries[i])
		}
	}
}

func TestAlwaysCannotColorProductionOrJSON(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	for _, tc := range []struct{ environment, format string }{
		{"production", "auto"}, {"production", "console"}, {"development", "json"}, {"test", "console"},
	} {
		t.Run(tc.environment+"/"+tc.format, func(t *testing.T) {
			settings := managerSettings(t)
			settings.Format, settings.Color, settings.Output = tc.format, "always", "console"
			var output bytes.Buffer
			m, err := New(Identity{Name: "test-service", Environment: tc.environment}, settings, &output, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			logger, err := m.Module("game")
			if err != nil {
				t.Fatal(err)
			}
			logger.Info("plain event")
			if err := m.Close(); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(output.String(), "\x1b") || strings.Contains(output.String(), "\\u001b") {
				t.Fatalf("always bypassed output safeguards: %s", &output)
			}
		})
	}
	// 编码器本身也执行隔离，防止未来装配时误传颜色开关。
	var output bytes.Buffer
	logger := zap.New(newCore("info", "json", "development", &output, time.UTC, true))
	logger.Info("JSON guard")
	if strings.Contains(output.String(), "\x1b") || strings.Contains(output.String(), "\\u001b") || readEntries(t, output.String())[0]["level"] != "info" {
		t.Fatal("JSON encoder accepted a colored level")
	}
}

func TestAutoAndNeverKeepRedirectedConsolePlain(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	for _, mode := range []string{"auto", "never"} {
		settings := managerSettings(t)
		settings.Format, settings.Color, settings.Output = "console", mode, "console"
		var output bytes.Buffer
		m := colorTestManager(t, settings, &output)
		if err := m.Close(); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(output.String(), "\x1b") || !strings.Contains(output.String(), "INFO") {
			t.Fatalf("mode=%s output=%q", mode, output.String())
		}
	}
}

func colorTestManager(t *testing.T, settings Options, output io.Writer) *Manager {
	t.Helper()
	m, err := newManager(Identity{Name: "test-service", Environment: "development"}, settings, output, io.Discard, newTestClock().now)
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
