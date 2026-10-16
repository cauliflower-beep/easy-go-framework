/**
 * @Author: LiuShuXin
 * @Description: 验证 Redis 命令可诊断性、任意命令名脱敏与批次摘要上限
 * @File: diagnostic_test.go
 * Software: Goland
 * @Date: 2026/10/14
 */
package redis

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cauliflower-beep/easy-go-framework/logging"
	goredis "github.com/redis/go-redis/v9"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestHookLogsSafeCommandNames(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	hook := commandHook{logger: zap.New(core), slow: time.Nanosecond}
	ctx := logging.WithRequestID(context.Background(), "diagnostic-request")
	for _, test := range []struct {
		args []any
		name string
	}{
		{[]any{"get", "private-key"}, "GET"},
		{[]any{"HGETALL", "private-key"}, "HGETALL"},
		{[]any{"cluster", "private-subcommand"}, "CLUSTER"},
		{[]any{"private-command-name", "private-value"}, "OTHER"},
		{[]any{"get\nprivate-command-name"}, "OTHER"},
		{nil, "OTHER"},
	} {
		logs.TakeAll()
		process := hook.ProcessHook(func(context.Context, goredis.Cmder) error {
			time.Sleep(time.Millisecond)
			return nil
		})
		if err := process(ctx, goredis.NewCmd(ctx, test.args...)); err != nil {
			t.Fatal(err)
		}
		entries := logs.FilterMessage("Redis command slow").All()
		if len(entries) != 1 {
			t.Fatalf("slow diagnostic count=%d", len(entries))
		}
		fields := entries[0].ContextMap()
		if fields["command"] != test.name || fields["command_count"] != int64(1) || fields["request_id"] != "diagnostic-request" {
			t.Fatalf("missing command identity: %v", fields)
		}
		encoded, err := json.Marshal(fields)
		if err != nil || strings.Contains(string(encoded), "private") {
			t.Fatal("command data or arbitrary verb entered diagnostic")
		}
	}
}

func TestHookLogsBoundedPipelineSummary(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	hook := commandHook{logger: zap.New(core)}
	ctx := context.Background()
	var cmds []goredis.Cmder
	for _, name := range []string{"get", "get", "set", "hgetall", "del", "incr", "expire", "ttl", "eval", "lpush", "private-command-name"} {
		cmds = append(cmds, goredis.NewCmd(ctx, name, "private-key", "private-value"))
	}
	for range 2000 {
		cmds = append(cmds, goredis.NewCmd(ctx, "get", "private-key"))
	}
	process := hook.ProcessPipelineHook(func(_ context.Context, cmds []goredis.Cmder) error {
		err := fixtureServerError("ERR private-script-body")
		for _, cmd := range cmds {
			cmd.SetErr(err)
		}
		return err
	})
	if err := process(ctx, cmds); err == nil || strings.Contains(err.Error(), "private") {
		t.Fatal("pipeline error unsafe or missing")
	}
	entries := logs.FilterMessage("Redis command failed").All()
	if len(entries) != 1 {
		t.Fatalf("pipeline diagnostic count=%d", len(entries))
	}
	fields := entries[0].ContextMap()
	encoded, err := json.Marshal(fields)
	if err != nil || strings.Contains(string(encoded), "private") || len(encoded) > 1500 {
		t.Fatal("pipeline summary unbounded or leaked command data")
	}
	var decoded struct {
		Count   int                `json:"command_count"`
		Types   []commandTypeCount `json:"command_types"`
		Omitted int                `json:"omitted_command_count"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Count != len(cmds) || len(decoded.Types) != maxDiagnosticCommandTypes || decoded.Types[0].Command != "GET" || decoded.Types[0].Count != 2002 || decoded.Omitted != 2 {
		t.Fatalf("incorrect aggregation: %+v", decoded)
	}
	for _, cmd := range cmds {
		if cmd.Err() == nil || strings.Contains(cmd.Err().Error(), "private") {
			t.Fatal("pipeline item error leaked")
		}
	}
}

func TestSuccessfulFastCommandDoesNotLog(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	hook := commandHook{logger: zap.New(core), slow: time.Hour}
	ctx := context.Background()
	if err := hook.ProcessHook(func(context.Context, goredis.Cmder) error { return nil })(ctx, goredis.NewCmd(ctx, "get", "private-key")); err != nil {
		t.Fatal(err)
	}
	if logs.Len() != 0 {
		t.Fatal("ordinary successful command produced diagnostic")
	}
}
