/**
 * @Author: LiuShuXin
 * @Description: 进程内 Redis 协议服务验证任务提交、查询、隔离与执行结果
 * @File: client_test.go
 * Software: Goland
 * @Date: 2026/10/15
 */
package taskqueue

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/cauliflower-beep/easy-go-framework/logging"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"go.uber.org/zap"
)

func fixture(t *testing.T) (*Client, Options, *miniredis.Miniredis) {
	t.Helper()
	r := miniredis.RunT(t)
	o := DefaultOptions()
	o.Redis.Host = r.Host()
	o.Redis.Port = r.Server().Addr().Port
	o.Redis.TLS = false
	o.Queue = "test-tasks"
	o.PollInterval = 10 * time.Millisecond
	o.DelayedCheckInterval = 20 * time.Millisecond
	o.RetryDelay = time.Second
	o.ShutdownTimeout = 2 * time.Second
	c, err := NewClient(o, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	return c, o, r
}
func worker(t *testing.T, o Options, handlers map[string]Handler) *Worker {
	t.Helper()
	w, err := NewWorker(o, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	for name, h := range handlers {
		if err = w.Register(name, h); err != nil {
			t.Fatal(err)
		}
	}
	if err = w.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := w.Close(); err != nil {
			t.Error(err)
		}
	})
	return w
}
func await(t *testing.T, c *Client, id string, state State) Info {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	var last Info
	var err error
	for time.Now().Before(deadline) {
		last, err = c.Get(context.Background(), id)
		if err == nil && last.State == state {
			return last
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("state not reached: wanted %s got %s error=%v", state, last.State, err)
	return Info{}
}

func TestEnqueueInspectAndIdentity(t *testing.T) {
	c, _, _ := fixture(t)
	ctx := context.Background()
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	in := Submission{ID: id, Type: "fixture.echo", Payload: json.RawMessage(`{"text":"<secret>&中文"}`)}
	info, err := c.Enqueue(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if info.State != StatePending || info.ID != id || info.MaxRetry != 0 {
		t.Fatal("incorrect queued metadata")
	}
	info, err = c.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if info.Type != in.Type || len(info.Result) != 0 {
		t.Fatal("incorrect inspection")
	}
	if _, err = c.Enqueue(ctx, in); !errors.Is(err, ErrIDConflict) {
		t.Fatalf("ID conflict %v", err)
	}
	if _, err = c.Get(ctx, uuid.NewString()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing task %v", err)
	}
	inspect := asynq.NewInspectorFromRedisClient(c.raw)
	raw, err := inspect.GetTaskInfo(c.options.Queue, id)
	if err != nil {
		t.Fatal(err)
	}
	if raw.Retention != c.options.Retention || raw.Timeout != c.options.TaskTimeout {
		t.Fatal("SDK defaults escaped explicit options")
	}
}

func TestHandlerResultAndRequestCorrelation(t *testing.T) {
	c, o, _ := fixture(t)
	requestID := "req_abcdefghijklmnopqrst"
	ctx := logging.WithRequestID(context.Background(), requestID)
	input := json.RawMessage(`{"text":"<secret>&中文"}`)
	worker(t, o, map[string]Handler{"fixture.echo": func(ctx context.Context, task Task) (json.RawMessage, error) {
		if task.ID == "" || task.Attempt != 1 || task.MaxRetry != 0 || string(task.Payload) != string(input) {
			t.Error("task data mismatch")
		}
		if logging.RequestID(ctx) != requestID {
			t.Error("request correlation lost")
		}
		return json.RawMessage(`{"artifact":"object-key"}`), nil
	}})
	info, err := c.Enqueue(ctx, Submission{Type: "fixture.echo", Payload: input})
	if err != nil {
		t.Fatal(err)
	}
	completed := await(t, c, info.ID, StateCompleted)
	if string(completed.Result) != `{"artifact":"object-key"}` || completed.CompletedAt.IsZero() {
		t.Fatal("result not retained")
	}
}

func TestTasksSurviveProducerRecreation(t *testing.T) {
	c, o, _ := fixture(t)
	first, err := c.Enqueue(context.Background(), Submission{Type: "fixture.echo"})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := NewClient(o, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	worker(t, o, map[string]Handler{"fixture.echo": func(context.Context, Task) (json.RawMessage, error) { return nil, nil }})
	await(t, next, first.ID, StateCompleted)
}

func TestDelayedTaskIsNotRunEarly(t *testing.T) {
	c, o, _ := fixture(t)
	ran := make(chan time.Time, 1)
	worker(t, o, map[string]Handler{"fixture.delayed": func(context.Context, Task) (json.RawMessage, error) { ran <- time.Now(); return nil, nil }})
	started := time.Now()
	info, err := c.Enqueue(context.Background(), Submission{Type: "fixture.delayed", Delay: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if info.State != StateScheduled || info.NextProcessAt.Before(started.Add(time.Second)) {
		t.Fatal("invalid scheduled time")
	}
	select {
	case actual := <-ran:
		if actual.Before(started.Add(time.Second)) {
			t.Fatal("task ran early")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("delayed task did not execute")
	}
	await(t, c, info.ID, StateCompleted)
}

func TestRetryAndPermanentFailure(t *testing.T) {
	for _, mode := range []string{"retry-success", "default-no-retry", "permanent"} {
		t.Run(mode, func(t *testing.T) {
			c, o, _ := fixture(t)
			var calls atomic.Int32
			worker(t, o, map[string]Handler{"fixture.retry": func(context.Context, Task) (json.RawMessage, error) {
				n := calls.Add(1)
				if mode == "permanent" {
					return nil, Permanent(errors.New("private failure"))
				}
				if n == 1 {
					return nil, errors.New("private failure")
				}
				return json.RawMessage(`"ok"`), nil
			}})
			in := Submission{Type: "fixture.retry"}
			if mode != "default-no-retry" {
				in.MaxRetry = 2
			}
			info, err := c.Enqueue(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			state := StateFailed
			if mode == "retry-success" {
				state = StateCompleted
			}
			out := await(t, c, info.ID, state)
			if mode == "retry-success" {
				if calls.Load() != 2 || out.Retried != 1 {
					t.Fatal("wrong retry count")
				}
			} else if calls.Load() != 1 {
				t.Fatal("unexpected retry")
			}
			if strings.Contains(out.LastFailure, "private") {
				t.Fatal("exception exposed")
			}
		})
	}
}

func TestQueueIsolationAndUnknownType(t *testing.T) {
	c, o, _ := fixture(t)
	worker(t, o, map[string]Handler{"fixture.known": func(context.Context, Task) (json.RawMessage, error) { return nil, nil }})
	unknown, err := c.Enqueue(context.Background(), Submission{Type: "fixture.unknown", MaxRetry: 3})
	if err != nil {
		t.Fatal(err)
	}
	failed := await(t, c, unknown.ID, StateFailed)
	if failed.LastFailure != "task handler not registered" || failed.Retried != 0 {
		t.Fatal("unknown type not terminal")
	}
	o.Queue = "other-tasks"
	other, err := NewClient(o, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err = other.Get(context.Background(), unknown.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross queue lookup %v", err)
	}
}
