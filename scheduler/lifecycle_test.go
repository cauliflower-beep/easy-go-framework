/**
 * @Author: LiuShuXin
 * @Description: 调度器停止准入、函数排空、取消与并发调用验证
 * @File: lifecycle_test.go
 * Software: Goland
 * @Date: 2026/10/15
 */
package scheduler

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func waitClosing(t *testing.T, s *Scheduler) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		closing := s.closing
		s.mu.Unlock()
		if closing {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("shutdown did not start")
}

func TestShutdownDrainsAndRejectsFutureTriggers(t *testing.T) {
	s := fixture(t)
	p := register(t, s, plan("drain"))
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var calls atomic.Int32
	p.handler = func(ctx context.Context) error {
		calls.Add(1)
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			t.Error("graceful drain canceled handler")
			return ctx.Err()
		}
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	go s.run(p)
	receive(t, entered)
	done := make(chan error, 8)
	for range 8 {
		go func() { done <- s.Shutdown(context.Background()) }()
	}
	waitClosing(t, s)
	s.run(p)
	if calls.Load() != 1 {
		t.Fatal("trigger admitted during shutdown")
	}
	select {
	case err := <-done:
		t.Fatalf("shutdown returned with active handler: %v", err)
	default:
	}
	once.Do(func() { close(release) })
	for range 8 {
		if err := receive(t, done); err != nil {
			t.Fatal(err)
		}
	}

}

func TestOnlyOverlappingHandlerIsSkipped(t *testing.T) {
	s := fixture(t)
	p := register(t, s, plan("overlap"))
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var calls atomic.Int32
	p.handler = func(ctx context.Context) error {
		calls.Add(1)
		entered <- struct{}{}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	go s.run(p)
	receive(t, entered)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() { s.run(p) })
	}
	wg.Wait()
	entry, _ := s.Get(p.plan.Name)
	if calls.Load() != 1 || entry.LastSkippedAt.IsZero() || !entry.Running {
		t.Fatal("handler overlap not skipped")
	}
	once.Do(func() { close(release) })
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		entry, _ = s.Get(p.plan.Name)
		if !entry.Running {
			break
		}
		time.Sleep(time.Millisecond)
	}
	s.run(p)
	receive(t, entered)
	if calls.Load() != 2 {
		t.Fatal("next trigger blocked after previous handler ended")
	}
}

func TestShutdownTimeoutAndCloseCancellation(t *testing.T) {
	for _, mode := range []string{"caller deadline", "configured deadline", "close"} {
		t.Run(mode, func(t *testing.T) {
			s := fixture(t)
			s.options.ShutdownTimeout = 60 * time.Millisecond
			p := register(t, s, plan("cancel"))
			entered := make(chan struct{})
			canceled := make(chan struct{})
			p.handler = func(ctx context.Context) error {
				close(entered)
				<-ctx.Done()
				close(canceled)
				return ctx.Err()
			}
			if err := s.Start(); err != nil {
				t.Fatal(err)
			}
			go s.run(p)
			receive(t, entered)
			if mode == "close" {
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				ctx := context.Background()
				var cancel context.CancelFunc
				if mode == "caller deadline" {
					ctx, cancel = context.WithTimeout(ctx, 15*time.Millisecond)
					defer cancel()
				}
				if err := s.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("deadline not honored %v", err)
				}
			}
			receive(t, canceled)
			receive(t, s.closed)
			entry, _ := s.Get(p.plan.Name)
			if entry.LastOutcome != OutcomeFailed || entry.LastFailure != "handler_canceled" {
				t.Fatalf("incorrect cancellation state: %+v", entry)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRunTimeout(t *testing.T) {
	s := fixture(t)
	s.options.RunTimeout = 20 * time.Millisecond
	p := register(t, s, plan("timeout"))
	p.handler = func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	s.run(p)
	entry, _ := s.Get(p.plan.Name)
	if entry.LastOutcome != OutcomeFailed || entry.LastFailure != "handler_deadline_exceeded" || entry.LastRunID == "" {
		t.Fatal("handler deadline state lost")
	}
}

func TestNonCooperativeHandlerHasBoundedWait(t *testing.T) {
	s := fixture(t)
	p := register(t, s, plan("uncooperative"))
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	p.handler = func(context.Context) error {
		close(entered)
		<-release
		return nil
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	go s.run(p)
	receive(t, entered)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := s.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("uncooperative wait not bounded")
	}
	select {
	case <-s.closed:
		t.Fatal("reported finished while callback still running")
	default:
	}
	once.Do(func() { close(release) })
	receive(t, s.closed)
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCloseBeforeStart(t *testing.T) {
	s := fixture(t)
	register(t, s, plan("never-started"))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); !errors.Is(err, ErrClosed) {
		t.Fatal("closed scheduler started")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPlanTimeoutAndLateNilReturn(t *testing.T) {
	s := fixture(t)
	p := plan("short-run")
	p.Timeout = 20 * time.Millisecond
	if err := s.Register(p, func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > p.Timeout {
			t.Error("plan deadline not applied")
		}
		<-ctx.Done()
		return nil // 超过期限即使函数返回 nil 也不能记录成功。
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	s.run(s.plans[p.Name])
	e, _ := s.Get(p.Name)
	if e.LastOutcome != OutcomeFailed || e.LastFailure != "handler_deadline_exceeded" || e.Running {
		t.Fatalf("incorrect timeout snapshot: %+v", e)
	}
}

func TestDifferentPlansRunConcurrently(t *testing.T) {
	s := fixture(t)
	entered := make(chan string, 2)
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	for _, name := range []string{"a", "b"} {
		if err := s.Register(plan(name), func(ctx context.Context) error {
			entered <- name
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	go s.run(s.plans["a"])
	go s.run(s.plans["b"])
	first, second := receive(t, entered), receive(t, entered)
	if first == second {
		t.Fatal("different plans not admitted")
	}
	once.Do(func() { close(release) })
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestHandlerFailureDoesNotDisableNextRun(t *testing.T) {
	s := fixture(t)
	p := register(t, s, plan("retry-next-period"))
	calls := 0
	p.handler = func(context.Context) error {
		calls++
		if calls == 1 {
			return errors.New("failed")
		}
		return nil
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	s.run(p)
	first, _ := s.Get(p.plan.Name)
	s.run(p)
	second, _ := s.Get(p.plan.Name)
	if calls != 2 || first.LastOutcome != OutcomeFailed || second.LastOutcome != OutcomeSucceeded || first.LastRunID == second.LastRunID || second.LastFailure != "" {
		t.Fatal("failed run blocked subsequent schedule")
	}
}
