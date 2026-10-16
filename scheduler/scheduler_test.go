/**
 * @Author: LiuShuXin
 * @Description: 无 Redis 依赖的 Cron 函数触发与静态计划验证
 * @File: scheduler_test.go
 * Software: Goland
 * @Date: 2026/10/15
 */
package scheduler

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

func fixture(t *testing.T) *Scheduler {
	t.Helper()
	s, err := New(DefaultOptions(), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}
func plan(name string) Plan { return Plan{Name: name, Spec: "@every 24h"} }
func register(t *testing.T, s *Scheduler, p Plan) *registered {
	t.Helper()
	if err := s.Register(p, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	return s.plans[p.Name]
}
func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(4 * time.Second):
		t.Fatal("expected event not observed")
		var zero T
		return zero
	}
}

func TestCronTriggersDistinctRunsWithoutRedis(t *testing.T) {
	s := fixture(t)
	p := plan("periodic")
	p.Spec = "@every 1s"
	jobs := make(chan string, 8)
	if err := s.Register(p, func(ctx context.Context) error {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("missing deadline")
		}
		jobs <- RunID(ctx)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	a, b := receive(t, jobs), receive(t, jobs)
	if a == b || uuid.Validate(a) != nil || uuid.Validate(b) != nil {
		t.Fatal("invalid run IDs")
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	entry, _ := s.Get(p.Name)
	if entry.LastOutcome != OutcomeSucceeded || entry.LastRunID == "" || entry.LastFinishedAt.Before(entry.LastStartedAt) || !entry.NextRun.IsZero() || entry.Running {
		t.Fatalf("invalid snapshot: %+v", entry)
	}
	if RunID(context.Background()) != "" || RunID(nil) != "" {
		t.Fatal("unexpected run ID outside handler")
	}
	select {
	case <-jobs:
		t.Fatal("extra execution")
	case <-time.After(60 * time.Millisecond):
	}
}

func TestRegistrationAndLifecycleStates(t *testing.T) {
	s := fixture(t)
	if err := s.Start(); err == nil {
		t.Fatal("empty scheduler started")
	}
	register(t, s, plan("z-plan"))
	register(t, s, plan("a-plan"))
	if err := s.Register(plan("a-plan"), func(context.Context) error { return nil }); !errors.Is(err, ErrDuplicate) {
		t.Fatal("duplicate plan accepted")
	}
	entries := s.Entries()
	if len(entries) != 2 || entries[0].Name != "a-plan" || !entries[0].NextRun.IsZero() || entries[0].LastOutcome != OutcomeNever {
		t.Fatal("invalid initial entries")
	}
	entries[0].Spec = "changed"
	original, _ := s.Get("a-plan")
	if original.Spec == "changed" {
		t.Fatal("snapshot mutated registration")
	}
	if _, err := s.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal("missing plan lookup")
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	running, _ := s.Get("a-plan")
	if running.NextRun.IsZero() {
		t.Fatal("next run unavailable")
	}
	if err := s.Start(); !errors.Is(err, ErrStarted) {
		t.Fatal("duplicate start accepted")
	}
	if err := s.Register(plan("late"), func(context.Context) error { return nil }); !errors.Is(err, ErrStarted) {
		t.Fatal("registration changed running schedule")
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); !errors.Is(err, ErrClosed) {
		t.Fatal("closed scheduler restarted")
	}
	if err := s.Register(plan("late"), func(context.Context) error { return nil }); !errors.Is(err, ErrClosed) {
		t.Fatal("closed registration accepted")
	}
}

func TestNoAutomaticCatchUp(t *testing.T) {
	s := fixture(t)
	p := register(t, s, plan("daily"))
	var calls atomic.Int32
	p.handler = func(context.Context) error {
		calls.Add(1)
		return nil
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatal("startup submitted an unscheduled catch-up task")
	}
}
