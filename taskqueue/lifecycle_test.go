/**
 * @Author: LiuShuXin
 * @Description: 提交与消费生命周期、并发执行和中断后再处理验证
 * @File: lifecycle_test.go
 * Software: Goland
 * @Date: 2026/10/15
 */
package taskqueue

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

func signal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("expected event not observed")
	}
}

func TestWorkerGracefulShutdown(t *testing.T) {
	c, o, _ := fixture(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	w := worker(t, o, map[string]Handler{"fixture.block": func(ctx context.Context, _ Task) (json.RawMessage, error) {
		close(entered)
		select {
		case <-release:
			return nil, nil
		case <-ctx.Done():
			t.Error("graceful shutdown canceled active task")
			return nil, ctx.Err()
		}
	}})
	info, err := c.Enqueue(context.Background(), Submission{Type: "fixture.block"})
	if err != nil {
		t.Fatal(err)
	}
	signal(t, entered)
	done := make(chan error, 1)
	go func() { done <- w.Shutdown(context.Background()) }()
	select {
	case err := <-done:
		t.Fatalf("returned before handler: %v", err)
	case <-time.After(60 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not complete")
	}
	await(t, c, info.ID, StateCompleted)
	if err = w.Register("fixture.extra", func(context.Context, Task) (json.RawMessage, error) { return nil, nil }); !errors.Is(err, ErrClosed) {
		t.Fatal("closed worker accepted registration")
	}
	if err = w.Start(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatal("closed worker restarted")
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerBoundedWaitAndRequeue(t *testing.T) {
	c, o, _ := fixture(t)
	o.ShutdownTimeout = 150 * time.Millisecond
	entered := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	w, err := NewWorker(o, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	if err = w.Register("fixture.resume", func(ctx context.Context, _ Task) (json.RawMessage, error) {
		close(entered)
		<-ctx.Done()
		close(canceled)
		<-release
		return nil, ctx.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if err = w.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	info, err := c.Enqueue(context.Background(), Submission{Type: "fixture.resume"})
	if err != nil {
		t.Fatal(err)
	}
	signal(t, entered)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err = w.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("bounded wait: %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("shutdown wait unbounded")
	}
	signal(t, canceled)
	await(t, c, info.ID, StatePending)
	once.Do(func() { close(release) })
	signal(t, w.done)
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	worker(t, o, map[string]Handler{"fixture.resume": func(context.Context, Task) (json.RawMessage, error) { return json.RawMessage(`"recovered"`), nil }})
	out := await(t, c, info.ID, StateCompleted)
	if string(out.Result) != `"recovered"` {
		t.Fatal("interrupted task did not resume")
	}
}

func TestTaskTimeoutCancelsHandler(t *testing.T) {
	c, o, _ := fixture(t)
	canceled := make(chan struct{})
	worker(t, o, map[string]Handler{"fixture.timeout": func(ctx context.Context, _ Task) (json.RawMessage, error) {
		<-ctx.Done()
		close(canceled)
		return nil, ctx.Err()
	}})
	info, err := c.Enqueue(context.Background(), Submission{Type: "fixture.timeout", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	signal(t, canceled)
	out := await(t, c, info.ID, StateFailed)
	if out.LastFailure != "task deadline exceeded" {
		t.Errorf("timeout reason %s", out.LastFailure)
	}
}

func TestConcurrencyLimit(t *testing.T) {
	c, o, _ := fixture(t)
	o.Concurrency = 2
	var active, maxActive atomic.Int32
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	worker(t, o, map[string]Handler{"fixture.concurrent": func(ctx context.Context, _ Task) (json.RawMessage, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for prior := maxActive.Load(); n > prior; prior = maxActive.Load() {
			if maxActive.CompareAndSwap(prior, n) {
				break
			}
		}
		entered <- struct{}{}
		select {
		case <-release:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}})
	var ids []string
	for range 8 {
		info, err := c.Enqueue(context.Background(), Submission{Type: "fixture.concurrent"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, info.ID)
	}
	signal(t, entered)
	signal(t, entered)
	if maxActive.Load() != 2 {
		t.Fatal("concurrency did not reach configured capacity")
	}
	once.Do(func() { close(release) })
	for _, id := range ids {
		await(t, c, id, StateCompleted)
	}
	if maxActive.Load() > 2 {
		t.Fatal("concurrency exceeded")
	}
}

type contextProbe struct {
	calls   atomic.Int32
	block   atomic.Bool
	entered chan struct{}
	once    sync.Once
}

func (h *contextProbe) DialHook(next goredis.DialHook) goredis.DialHook { return next }
func (h *contextProbe) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return next
}
func (h *contextProbe) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error {
		if cmd.Name() == "sismember" || cmd.Name() == "evalsha" || cmd.Name() == "eval" {
			if _, ok := ctx.Deadline(); !ok {
				return errors.New("missing query deadline")
			}
			h.calls.Add(1)
			if h.block.Load() {
				h.once.Do(func() { close(h.entered) })
				<-ctx.Done()
				return ctx.Err()
			}
		}
		return next(ctx, cmd)
	}
}

func TestInspectorContextAndClientDrain(t *testing.T) {
	c, _, _ := fixture(t)
	info, err := c.Enqueue(context.Background(), Submission{Type: "fixture.query"})
	if err != nil {
		t.Fatal(err)
	}
	probe := &contextProbe{entered: make(chan struct{})}
	c.raw.AddHook(probe)
	if _, err = c.Get(context.Background(), info.ID); err != nil {
		t.Fatal(err)
	}
	if probe.calls.Load() < 2 {
		t.Fatal("context did not cover complete inspector query")
	}
	probe.block.Store(true)
	readDone := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	go func() { _, err := c.Get(ctx, info.ID); readDone <- err }()
	signal(t, probe.entered)
	shutdown := make(chan error, 1)
	go func() { shutdown <- c.Shutdown(context.Background()) }()
	if err = <-readDone; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("query deadline %v", err)
	}
	if err = <-shutdown; err != nil {
		t.Fatal(err)
	}
	if _, err = c.Enqueue(context.Background(), Submission{Type: "fixture.query"}); !errors.Is(err, ErrClosed) {
		t.Fatal("closed client accepted task")
	}
}

func TestClientForceCloseAndCanceledInput(t *testing.T) {
	c, _, _ := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Enqueue(ctx, Submission{Type: "fixture.noop"}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := c.Get(nil, "id"); err == nil {
		t.Fatal("nil context accepted")
	}
	info, err := c.Enqueue(context.Background(), Submission{Type: "fixture.query"})
	if err != nil {
		t.Fatal(err)
	}
	probe := &contextProbe{entered: make(chan struct{})}
	probe.block.Store(true)
	c.raw.AddHook(probe)
	result := make(chan error, 1)
	go func() { _, err := c.Get(context.Background(), info.ID); result <- err }()
	signal(t, probe.entered)
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	if err = <-result; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestWorkerRegistrationAndStartup(t *testing.T) {
	_, o, r := fixture(t)
	w, err := NewWorker(o, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err = w.Start(context.Background()); err == nil {
		t.Fatal("empty worker started")
	}
	h := func(context.Context, Task) (json.RawMessage, error) { return nil, nil }
	if err = w.Register("fixture.test", nil); err == nil {
		t.Fatal("nil handler accepted")
	}
	if err = w.Register("fixture.test", h); err != nil {
		t.Fatal(err)
	}
	if err = w.Register("fixture.test", h); err == nil {
		t.Fatal("duplicate handler accepted")
	}
	r.RequireAuth("private-password")
	if err = w.Start(context.Background()); err == nil {
		t.Fatal("startup authentication failure hidden")
	}
	r.RequireAuth("")
	if err = w.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = w.Start(context.Background()); !errors.Is(err, ErrStarted) {
		t.Fatal("duplicate start accepted")
	}
	if err = w.Register("fixture.other", h); !errors.Is(err, ErrStarted) {
		t.Fatal("late registration accepted")
	}
}
