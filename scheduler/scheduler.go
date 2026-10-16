/**
 * @Author: LiuShuXin
 * @Description: 独立周期函数调度、运行快照及有界关闭
 * @File: scheduler.go
 * Software: Goland
 * @Date: 2026/10/15
 */
package scheduler

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/cauliflower-beep/easy-go-framework/logging"
	"github.com/google/uuid"
	"github.com/robfig/cron/v3"
	"go.uber.org/zap"
)

type Outcome string

const (
	OutcomeNever     Outcome = "never"
	OutcomeRunning   Outcome = "running"
	OutcomeSucceeded Outcome = "succeeded"
	OutcomeFailed    Outcome = "failed"
)

// Handler 在调度进程内执行，必须遵守 Context 取消；返回 nil 表示函数完成。
// 同一计划不重叠调用，不同计划可并发；无需也不应自行启动 goroutine 来完成本次工作。
type Handler func(context.Context) error

type runIDKey struct{}

// RunID 返回本次调用的 UUID；队列适配层可用它作为任务 ID。调度调用之外返回空串。
func RunID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(runIDKey{}).(string)
	return id
}

// Entry 只描述当前进程的函数调用；函数若仅提交队列，成功不表示后台任务完成。
type Entry struct {
	Name           string
	Spec           string
	TimeZone       string
	NextRun        time.Time
	LastStartedAt  time.Time
	LastFinishedAt time.Time
	LastRunID      string
	LastOutcome    Outcome
	LastFailure    string
	LastSkippedAt  time.Time
	Running        bool
}

type registered struct {
	plan     Plan
	schedule cron.Schedule
	engineID cron.EntryID
	entry    Entry
	handler  Handler
}
type dispatch struct {
	plan    *registered
	ctx     context.Context
	cancel  context.CancelFunc
	started time.Time
	runID   string
}

// Scheduler 无外部存储依赖；每组计划部署时只启用一个调度实例。
// 注册在 Start 前完成，启动后不能动态修改；停止后不能重启同一对象。
type Scheduler struct {
	options     Options
	engine      *cron.Cron
	logger      *zap.Logger
	mu          sync.Mutex
	plans       map[string]*registered
	started     bool
	closing     bool
	active      map[*dispatch]struct{}
	drained     chan struct{}
	drainedOnce sync.Once
	stopOnce    sync.Once
	closed      chan struct{}
}

func New(o Options, logger *zap.Logger) (*Scheduler, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		return nil, errors.New("scheduler requires a logger")
	}
	location, _ := loadLocation(o.TimeZone)
	return &Scheduler{options: o, engine: cron.New(cron.WithLocation(location), cron.WithLogger(engineLogger{logger})), logger: logger, plans: make(map[string]*registered), active: make(map[*dispatch]struct{}), drained: make(chan struct{}), closed: make(chan struct{})}, nil
}

func (s *Scheduler) Register(plan Plan, handler Handler) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return SafeError(ErrClosed)
	}
	if s.started {
		return SafeError(ErrStarted)
	}
	schedule, plan, err := compile(plan, s.options.TimeZone)
	if err != nil {
		return err
	}
	if handler == nil {
		return errors.New("scheduler requires a handler")
	}
	if _, exists := s.plans[plan.Name]; exists {
		return SafeError(ErrDuplicate)
	}
	p := &registered{plan: plan, schedule: schedule, handler: handler, entry: Entry{Name: plan.Name, Spec: plan.Spec, TimeZone: plan.TimeZone, LastOutcome: OutcomeNever}}
	p.engineID = s.engine.Schedule(schedule, cron.FuncJob(func() { s.run(p) }))
	s.plans[plan.Name] = p
	return nil
}

// Start 开始未来的触发，不立即执行或补跑计划。
func (s *Scheduler) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return SafeError(ErrClosed)
	}
	if s.started {
		return SafeError(ErrStarted)
	}
	if len(s.plans) == 0 {
		return errors.New("scheduler requires at least one plan")
	}
	s.started = true
	s.engine.Start()
	return nil
}

func (s *Scheduler) run(p *registered) {
	s.mu.Lock()
	if !s.started || s.closing {
		s.mu.Unlock()
		return
	}
	if p.entry.Running {
		p.entry.LastSkippedAt = time.Now().UTC()
		s.mu.Unlock()
		s.logger.Warn("scheduler trigger skipped while handler is running", zap.String("plan", p.plan.Name))
		return
	}
	timeout := p.plan.Timeout
	if timeout == 0 {
		timeout = s.options.RunTimeout
	}
	work, cancel := context.WithTimeout(context.Background(), timeout)
	op := &dispatch{plan: p, ctx: work, cancel: cancel, started: time.Now()}
	p.entry.Running = true
	p.entry.LastStartedAt = op.started.UTC()
	p.entry.LastFinishedAt = time.Time{}
	p.entry.LastOutcome = OutcomeRunning
	p.entry.LastFailure = ""
	p.entry.LastRunID = ""
	s.active[op] = struct{}{}
	s.mu.Unlock()
	var err error
	defer func() {
		if recover() != nil {
			err = errPanic
		}
		s.finish(op, errors.Join(err, op.ctx.Err()))
	}()
	id, err := uuid.NewRandom()
	if err != nil {
		return
	}
	op.runID = id.String()
	op.ctx = context.WithValue(op.ctx, runIDKey{}, op.runID)
	s.mu.Lock()
	p.entry.LastRunID = op.runID
	s.mu.Unlock()
	err = p.handler(op.ctx)
}

func (s *Scheduler) finish(op *dispatch, err error) {
	op.cancel()
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		op.plan.entry.Running = false
		delete(s.active, op)
		s.signalDrained()
	}()
	s.mu.Lock()
	if err == nil {
		op.plan.entry.LastOutcome = OutcomeSucceeded
		op.plan.entry.LastFailure = ""
	} else {
		op.plan.entry.LastOutcome = OutcomeFailed
		op.plan.entry.LastFailure = failureReason(err)
	}
	op.plan.entry.LastFinishedAt = time.Now().UTC()
	s.mu.Unlock()
	fields := []zap.Field{zap.String("plan", op.plan.plan.Name), zap.String("run_id", op.runID), zap.Duration("duration", time.Since(op.started))}
	if err != nil {
		fields = append(fields, logging.ErrorFields(SafeError(err))...)
		s.logger.Warn("scheduler handler failed", fields...)
	} else {
		s.logger.Info("scheduler handler completed", fields...)
	}
}

// Entries 按名称排序；NextRun 在启动前及停止后为空，其余快照在当前进程内保留。
func (s *Scheduler) Entries() []Entry {
	s.mu.Lock()
	running := s.started && !s.closing
	items := make([]Entry, 0, len(s.plans))
	ids := make(map[string]cron.EntryID, len(s.plans))
	for name, p := range s.plans {
		items = append(items, p.entry)
		ids[name] = p.engineID
	}
	s.mu.Unlock()
	if running {
		native := s.engine.Entries()
		next := make(map[cron.EntryID]time.Time, len(native))
		for _, entry := range native {
			next[entry.ID] = entry.Next
		}
		for i := range items {
			items[i].NextRun = next[ids[items[i].Name]]
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return items
}
func (s *Scheduler) Get(name string) (Entry, error) {
	for _, entry := range s.Entries() {
		if entry.Name == name {
			return entry, nil
		}
	}
	return Entry{}, SafeError(ErrNotFound)
}

func (s *Scheduler) signalDrained() {
	if s.closing && len(s.active) == 0 {
		s.drainedOnce.Do(func() { close(s.drained) })
	}
}
func (s *Scheduler) beginDrain() {
	s.mu.Lock()
	s.closing = true
	s.signalDrained()
	s.mu.Unlock()
	s.stopOnce.Do(func() { go func() { <-s.engine.Stop().Done(); <-s.drained; close(s.closed) }() })
}
func (s *Scheduler) cancelActive() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for op := range s.active {
		op.cancel()
	}
}
func (s *Scheduler) wait(ctx context.Context) error {
	select {
	case <-s.closed:
		return nil
	default:
	}
	select {
	case <-s.closed:
		return nil
	case <-ctx.Done():
		s.cancelActive()
		return SafeError(ctx.Err())
	}
}

// Shutdown 停止未来触发，等待在途函数与日志完成；期限到后取消 Context。
// 无法强制终止不响应取消的函数；超时返回后仍须保留其使用的依赖。
func (s *Scheduler) Shutdown(ctx context.Context) error {
	if ctx == nil {
		return errors.New("scheduler shutdown context must not be nil")
	}
	work, cancel := context.WithTimeout(ctx, s.options.ShutdownTimeout)
	defer cancel()
	s.beginDrain()
	return s.wait(work)
}

// Close 立即停止调度并取消在途函数，再在配置期限内等待。
func (s *Scheduler) Close() error {
	s.beginDrain()
	s.cancelActive()
	work, cancel := context.WithTimeout(context.Background(), s.options.ShutdownTimeout)
	defer cancel()
	return s.wait(work)
}

type engineLogger struct{ logger *zap.Logger }

func (engineLogger) Info(string, ...any) {}
func (l engineLogger) Error(err error, _ string, _ ...any) {
	l.logger.Warn("scheduler engine error", logging.ErrorFields(SafeError(err))...)
}
