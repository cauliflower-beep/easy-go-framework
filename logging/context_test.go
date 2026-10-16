/**
 * @Author: LiuShuXin
 * @Description: 验证请求上下文关联和共享模块日志隔离
 * @File: context_test.go
 * Software: Goland
 * @Date: 2026/10/10 11:10
 */

package logging

import (
	"bytes"
	"context"
	"testing"

	"go.uber.org/zap"
)

func TestRequestContextLoggerIsolation(t *testing.T) {
	var output bytes.Buffer
	settings := managerSettings(t)
	settings.Output = "console"
	m := testManager(t, settings, &output, newTestClock())
	module, err := m.Module("game")
	if err != nil {
		t.Fatal(err)
	}
	first := WithRequestID(context.Background(), "first")
	second := WithRequestID(context.Background(), "second")
	ForRequest(module, first).Info("one", zap.String("step", "handler"))
	ForRequest(module, second).Info("two")
	ForRequest(module, context.Background()).Info("background")
	entries := readEntries(t, output.String())[1:]
	if len(entries) != 3 || entries[0]["request_id"] != "first" || entries[1]["request_id"] != "second" || entries[2]["request_id"] != nil || entries[0]["module"] != "game" || RequestID(first) != "first" {
		t.Fatalf("request context leaked: %v", entries)
	}
}
