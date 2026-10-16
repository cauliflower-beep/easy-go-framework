/**
 * @Author: LiuShuXin
 * @Description: 任务失败、panic、畸形载荷及结果的安全诊断验证
 * @File: diagnostic_test.go
 * Software: Goland
 * @Date: 2026/10/15
 */
package taskqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cauliflower-beep/easy-go-framework/logging"
	"github.com/hibiken/asynq"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestPanicFailureAndResultPrivacy(t *testing.T) {
	for _, mode := range []string{"error", "panic", "result-limit", "invalid-result"} {
		t.Run(mode, func(t *testing.T) {
			c, o, _ := fixture(t)
			o.MaxResultBytes = 16
			core, logs := observer.New(zap.DebugLevel)
			w, err := NewWorker(o, zap.New(core))
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()
			if err = w.Register("fixture.private", func(context.Context, Task) (json.RawMessage, error) {
				switch mode {
				case "panic":
					panic("private-panic-value")
				case "result-limit":
					return json.RawMessage(`"private-result-that-exceeds-limit"`), nil
				case "invalid-result":
					return json.RawMessage("private-invalid-result"), nil
				default:
					return nil, errors.New("private-domain-error")
				}
			}); err != nil {
				t.Fatal(err)
			}
			if err = w.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			info, err := c.Enqueue(context.Background(), Submission{Type: "fixture.private", Payload: json.RawMessage(`{"secret":"private-payload"}`)})
			if err != nil {
				t.Fatal(err)
			}
			out := await(t, c, info.ID, StateFailed)
			if len(out.Result) != 0 || strings.Contains(out.LastFailure, "private-") {
				t.Fatal("failed task exposed private result")
			}
			for _, entry := range logs.All() {
				if strings.Contains(fmt.Sprint(entry.ContextMap(), entry.Message), "private-") {
					t.Fatal("private log")
				}
			}
			inspect := asynq.NewInspectorFromRedisClient(c.raw)
			stored, err := inspect.GetTaskInfo(o.Queue, info.ID)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(stored.LastErr, "private-") {
				t.Fatal("private exception persisted")
			}
		})
	}
}

func TestMalformedQueuePayloadIsTerminal(t *testing.T) {
	c, o, _ := fixture(t)
	worker(t, o, map[string]Handler{"fixture.decode": func(context.Context, Task) (json.RawMessage, error) {
		t.Error("invalid payload reached handler")
		return nil, nil
	}})
	info, err := c.backend.EnqueueContext(context.Background(), asynq.NewTask("fixture.decode", []byte(`{"version":999,"data":"cHJpdmF0ZQ=="}`)), asynq.Queue(o.Queue), asynq.MaxRetry(3))
	if err != nil {
		t.Fatal(err)
	}
	out := await(t, c, info.ID, StateFailed)
	if out.Retried != 0 || out.LastFailure != "invalid task payload" {
		t.Fatal("malformed task not terminal")
	}
}

func TestSafeErrorsAndFormatting(t *testing.T) {
	for cause, target := range map[error]error{asynq.ErrTaskNotFound: ErrNotFound, asynq.ErrQueueNotFound: ErrNotFound, asynq.ErrTaskIDConflict: ErrIDConflict, context.Canceled: context.Canceled, context.DeadlineExceeded: context.DeadlineExceeded, ErrClosed: ErrClosed} {
		err := SafeError(fmt.Errorf("private-wrapper: %w", cause))
		if !errors.Is(err, target) {
			t.Fatal("sentinel lost")
		}
		if strings.Contains(fmt.Sprintf("%v %+v %#v", err, err, err), "private-") {
			t.Fatal("error format leaked")
		}
		entries, _ := logging.Diagnostics(err)
		if strings.Contains(fmt.Sprint(entries), "private-") {
			t.Fatal("error diagnostic leaked")
		}
	}
	if Permanent(nil) != nil || !errors.Is(Permanent(errors.New("private")), ErrSkipRetry) {
		t.Fatal("permanent error semantics")
	}
	o := DefaultOptions()
	o.Redis.Password = "private-password"
	for _, value := range []any{o, Submission{Payload: json.RawMessage(`"private-payload"`)}, Task{Payload: json.RawMessage(`"private-payload"`)}, Info{Result: json.RawMessage(`"private-result"`)}} {
		if strings.Contains(fmt.Sprintf("%v %+v %#v", value, value, value), "private-") {
			t.Fatal("object format leaked")
		}
	}
	encoded, _ := json.Marshal(o)
	if strings.Contains(string(encoded), "private-password") {
		t.Fatal("password serialized")
	}
	core, logs := observer.New(zap.DebugLevel)
	log := sdkLogger{zap.New(core)}
	log.Warn("private-payload")
	log.Error(errors.New("private-error"))
	log.Fatal("private-fatal")
	for _, entry := range logs.All() {
		if strings.Contains(fmt.Sprint(entry.ContextMap(), entry.Message), "private-") {
			t.Fatal("SDK logger leaked")
		}
	}
}
