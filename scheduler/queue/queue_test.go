/**
 * @Author: LiuShuXin
 * @Description: 可选队列适配的配置验证、唯一任务标识及后台执行往返
 * @File: queue_test.go
 * Software: Goland
 * @Date: 2026/10/15
 */
package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/cauliflower-beep/easy-go-framework/config"
	"github.com/cauliflower-beep/easy-go-framework/scheduler"
	"github.com/cauliflower-beep/easy-go-framework/taskqueue"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

func fixture(t *testing.T) (*scheduler.Scheduler, *taskqueue.Client, taskqueue.Options, *miniredis.Miniredis) {
	t.Helper()
	r := miniredis.RunT(t)
	o := taskqueue.DefaultOptions()
	o.Redis.Host = r.Host()
	o.Redis.Port = r.Server().Addr().Port
	o.Redis.TLS = false
	o.Queue = "scheduler-adapter"
	o.PollInterval = 10 * time.Millisecond
	o.DelayedCheckInterval = 20 * time.Millisecond
	c, err := taskqueue.NewClient(o, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	s, err := scheduler.New(scheduler.DefaultOptions(), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s, c, o, r
}
func plan() Plan {
	return Plan{Schedule: scheduler.Plan{Name: "queued", Spec: "@every 1s", Timeout: 5 * time.Second}, TaskType: "fixture.echo", Payload: `{"message":"hello"}`}
}
func waitEntry(t *testing.T, s *scheduler.Scheduler, want scheduler.Outcome) scheduler.Entry {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		e, err := s.Get("queued")
		if err != nil {
			t.Fatal(err)
		}
		if e.LastOutcome == want && !e.Running {
			return e
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("expected scheduler outcome not observed")
	return scheduler.Entry{}
}

func TestCronToWorkerRoundTripAndDistinctTaskIDs(t *testing.T) {
	s, c, o, _ := fixture(t)
	w, err := taskqueue.NewWorker(o, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	processed := make(chan taskqueue.Task, 8)
	if err = w.Register("fixture.echo", func(_ context.Context, in taskqueue.Task) (json.RawMessage, error) {
		processed <- in
		return in.Payload, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = w.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := w.Close(); err != nil {
			t.Error(err)
		}
	}()
	p := plan()
	if err = Register(s, c, p); err != nil {
		t.Fatal(err)
	}
	if err = s.Start(); err != nil {
		t.Fatal(err)
	}
	var tasks []taskqueue.Task
	for range 2 {
		select {
		case task := <-processed:
			tasks = append(tasks, task)
		case <-time.After(4 * time.Second):
			t.Fatal("scheduled task not processed")
		}
	}
	if err = s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	e, _ := s.Get(p.Schedule.Name)
	if e.LastOutcome != scheduler.OutcomeSucceeded || e.LastRunID != tasks[1].ID || tasks[0].ID == tasks[1].ID || uuid.Validate(e.LastRunID) != nil {
		t.Fatalf("invalid ID mapping: %+v", e)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		info, err := c.Get(context.Background(), e.LastRunID)
		if err != nil {
			t.Fatal(err)
		}
		if info.State == taskqueue.StateCompleted {
			if string(info.Result) != p.Payload {
				t.Fatal("result differs")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("result not retained")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = c.Ping(context.Background()); err != nil {
		t.Fatal("borrowed client closed by scheduler")
	}
}

func TestQueueSubmissionDoesNotWaitForWorker(t *testing.T) {
	s, c, _, _ := fixture(t)
	if err := Register(s, c, plan()); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	e := waitEntry(t, s, scheduler.OutcomeSucceeded)
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	info, err := c.Get(context.Background(), e.LastRunID)
	if err != nil {
		t.Fatal(err)
	}
	if info.State != taskqueue.StatePending {
		t.Fatalf("expected pending without worker: %v", info.State)
	}
}

func TestValidationWithoutRedisIOAndPayloadPrivacy(t *testing.T) {
	s, c, _, r := fixture(t)
	if Register(nil, c, plan()) == nil || Register(s, nil, plan()) == nil {
		t.Fatal("nil dependency accepted")
	}
	for name, mutate := range map[string]func(*Plan){
		"type":     func(p *Plan) { p.TaskType = "bad type" },
		"json":     func(p *Plan) { p.Payload = "private-invalid-json" },
		"size":     func(p *Plan) { p.Payload = `"` + strings.Repeat("x", 65<<10) + `"` },
		"timeout":  func(p *Plan) { p.TaskTimeout = time.Millisecond },
		"retry":    func(p *Plan) { p.MaxRetry = -1 },
		"schedule": func(p *Plan) { p.Schedule.Spec = "invalid" },
	} {
		t.Run(name, func(t *testing.T) {
			p := plan()
			mutate(&p)
			if Register(s, c, p) == nil {
				t.Fatal("invalid plan accepted")
			}
		})
	}
	p := plan()
	p.Payload = `{"secret":"private-payload"}`
	if err := Register(s, c, p); err != nil {
		t.Fatal(err)
	}
	if len(r.Keys()) != 0 {
		t.Fatal("registration performed Redis IO")
	}
	if strings.Contains(fmt.Sprintf("%v %+v %#v", p, p, p), "private-payload") {
		t.Fatal("payload formatting leaked")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	e := waitEntry(t, s, scheduler.OutcomeFailed)
	if e.LastRunID == "" || e.LastFailure != "handler_failed" {
		t.Fatal("failed submission ID lost")
	}
}

func TestQueueYAMLComposition(t *testing.T) {
	type configuration struct {
		Plans []Plan `mapstructure:"plans"`
	}
	file := filepath.Join(t.TempDir(), "plans.yaml")
	data := []byte("plans:\n  - schedule:\n      name: queued\n      spec: '@every 1s'\n      timeout: 5s\n    task_type: fixture.echo\n    payload: '{\"message\":\"hello\"}'\n    task_timeout: 2m\n    max_retry: 1\n")
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	var cfg configuration
	if err := config.Load(file, &cfg, config.Options{EnvPrefix: "QUEUE_SCHEDULER_FIXTURE"}); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Plans) != 1 || cfg.Plans[0].Schedule.Timeout != 5*time.Second || cfg.Plans[0].TaskTimeout != 2*time.Minute || cfg.Plans[0].MaxRetry != 1 {
		t.Fatal("configuration decoding failed")
	}
	s, c, _, _ := fixture(t)
	if err := Register(s, c, cfg.Plans[0]); err != nil {
		t.Fatal(err)
	}
}
