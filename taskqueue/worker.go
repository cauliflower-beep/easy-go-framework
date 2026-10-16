/**
 * @Author: LiuShuXin
 * @Description: 业务处理函数注册、消费执行、受控重试及消费者生命周期
 * @File: worker.go
 * Software: Goland
 * @Date: 2026/10/15
 */
package taskqueue

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cauliflower-beep/easy-go-framework/logging"
	"github.com/hibiken/asynq"
	goredis "github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// Worker 可与 Client 同进程运行；注册完成后调用 Start，不接管进程信号。
type Worker struct {
	options     Options
	raw         *goredis.Client
	logger      *zap.Logger
	mu          sync.Mutex
	handlers    map[string]Handler
	starting    bool
	started     bool
	closing     bool
	backendDone bool
	server      *asynq.Server
	handlersWG  sync.WaitGroup
	stopOnce    sync.Once
	done        chan struct{}
	closeErr    error
}

func NewWorker(o Options, logger *zap.Logger) (*Worker, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		return nil, errors.New("taskqueue logger must not be nil")
	}
	settings, err := o.Redis.DriverOptions()
	if err != nil {
		return nil, err
	}
	return &Worker{options: o, raw: goredis.NewClient(settings), logger: logger, handlers: make(map[string]Handler), done: make(chan struct{})}, nil
}

func (w *Worker) Register(taskType string, handler Handler) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closing {
		return SafeError(ErrClosed)
	}
	if w.starting || w.started {
		return SafeError(ErrStarted)
	}
	if !namePattern.MatchString(taskType) || handler == nil {
		return errors.New("taskqueue registration requires valid type and nonnil handler")
	}
	if _, exists := w.handlers[taskType]; exists {
		return errors.New("taskqueue handler already registered")
	}
	w.handlers[taskType] = handler
	return nil
}

// Start 的 Context 只约束启动连通性检查。任务执行与提交请求/启动 Context 解耦。
// 启动失败可重新尝试；成功启动后不能重启同一个 Worker。
func (w *Worker) Start(ctx context.Context) (err error) {
	if ctx == nil {
		return errors.New("taskqueue startup context must not be nil")
	}
	w.mu.Lock()
	if w.closing {
		w.mu.Unlock()
		return SafeError(ErrClosed)
	}
	if w.starting || w.started {
		w.mu.Unlock()
		return SafeError(ErrStarted)
	}
	if len(w.handlers) == 0 {
		w.mu.Unlock()
		return errors.New("taskqueue worker requires at least one handler")
	}
	w.starting = true
	w.mu.Unlock()
	defer func() { w.mu.Lock(); w.starting = false; w.mu.Unlock() }()
	work, cancel := context.WithTimeout(ctx, w.options.Redis.HealthTimeout)
	defer cancel()
	if err = errors.Join(w.raw.Ping(work).Err(), work.Err()); err != nil {
		w.mu.Lock()
		if w.closing {
			err = errors.Join(ErrClosed, err)
		}
		w.mu.Unlock()
		return SafeError(err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closing {
		return SafeError(ErrClosed)
	}
	w.server = asynq.NewServerFromRedisClient(w.raw, asynq.Config{
		Concurrency: w.options.Concurrency, Queues: map[string]int{w.options.Queue: 1}, TaskCheckInterval: w.options.PollInterval,
		DelayedTaskCheckInterval: w.options.DelayedCheckInterval, ShutdownTimeout: w.options.ShutdownTimeout,
		RetryDelayFunc: func(_ int, _ error, _ *asynq.Task) time.Duration { return w.options.RetryDelay },
		Logger:         sdkLogger{w.logger}, LogLevel: asynq.WarnLevel,
		ErrorHandler: asynq.ErrorHandlerFunc(w.executionError),
		HealthCheckFunc: func(err error) {
			if err != nil {
				w.logger.Warn("taskqueue backend health check failed", logging.ErrorFields(SafeError(err))...)
			}
		},
	})
	if err = w.server.Start(asynq.HandlerFunc(w.perform)); err != nil {
		return SafeError(err)
	}
	w.started = true
	w.logger.Info("taskqueue worker started", zap.String("queue", w.options.Queue), zap.Int("concurrency", w.options.Concurrency))
	return nil
}

type executionError struct {
	label     string
	permanent bool
}

func (e *executionError) Error() string        { return e.label }
func (e *executionError) Is(target error) bool { return e.permanent && target == asynq.SkipRetry }

func (w *Worker) perform(ctx context.Context, raw *asynq.Task) (err error) {
	w.mu.Lock()
	if w.backendDone {
		w.mu.Unlock()
		return &executionError{label: "task execution canceled"}
	}
	w.handlersWG.Add(1)
	handler := w.handlers[raw.Type()]
	w.mu.Unlock()
	defer w.handlersWG.Done()
	// 在 SDK 之前恢复 panic，既不持久化 panic 值，也不让它进入 SDK 格式化日志。
	defer func() {
		if recover() != nil {
			err = &executionError{label: "task handler panicked"}
		}
	}()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if handler == nil {
		return &executionError{label: "task handler not registered", permanent: true}
	}
	if len(raw.Payload()) > base64.StdEncoding.EncodedLen(w.options.MaxPayloadBytes)+256 {
		return &executionError{label: "task payload exceeds limit", permanent: true}
	}
	var body envelope
	if json.Unmarshal(raw.Payload(), &body) != nil || body.Version != 1 || !json.Valid(body.Data) || !utf8.Valid(body.Data) {
		return &executionError{label: "invalid task payload", permanent: true}
	}
	if len(body.Data) > w.options.MaxPayloadBytes {
		return &executionError{label: "task payload exceeds limit", permanent: true}
	}
	if validCorrelation(body.RequestID) {
		ctx = logging.WithRequestID(ctx, body.RequestID)
	}
	id, _ := asynq.GetTaskID(ctx)
	retried, _ := asynq.GetRetryCount(ctx)
	maxRetry, _ := asynq.GetMaxRetry(ctx)
	result, err := handler(ctx, Task{ID: id, Type: raw.Type(), Payload: append(json.RawMessage(nil), body.Data...), Attempt: retried + 1, MaxRetry: maxRetry})
	// Context 到期与处理函数返回会竞争 SDK 的完成分支。优先保留执行期限，
	// 避免同一超时随机被记录为普通处理失败。
	if canceled := ctx.Err(); canceled != nil {
		return canceled
	}
	if err != nil {
		return &executionError{label: "task handler failed", permanent: errors.Is(err, ErrSkipRetry)}
	}
	if len(result) == 0 {
		result = json.RawMessage("null")
	}
	if len(result) > w.options.MaxResultBytes {
		return &executionError{label: "task result exceeds limit", permanent: true}
	}
	if !json.Valid(result) || !utf8.Valid(result) {
		return &executionError{label: "invalid task result", permanent: true}
	}
	_, err = raw.ResultWriter().Write(result)
	if err != nil {
		return &executionError{label: "task result persistence failed"}
	}
	return nil
}

func (w *Worker) executionError(ctx context.Context, task *asynq.Task, err error) {
	id, _ := asynq.GetTaskID(ctx)
	retried, _ := asynq.GetRetryCount(ctx)
	maxRetry, _ := asynq.GetMaxRetry(ctx)
	taskType := task.Type()
	if !namePattern.MatchString(taskType) {
		taskType = "OTHER"
	}
	// SDK 回调拿到的上下文不含 envelope 请求关联；这里仅记录执行标识与安全分类。
	w.logger.Warn("taskqueue attempt failed", zap.String("task_id", id), zap.String("task_type", taskType), zap.Int("attempt", retried+1), zap.Int("max_retry", maxRetry), zap.String("reason", failureLabel(err.Error())))
}

func (w *Worker) beginShutdown() {
	w.stopOnce.Do(func() {
		w.mu.Lock()
		w.closing = true
		server := w.server
		started := w.started
		w.mu.Unlock()
		go func() {
			// 让 SDK 完成租约和重新入队，不能提前关闭 Redis 或取消 BaseContext；
			// 否则正常停机可能被当成普通业务失败归档。
			if started {
				server.Shutdown()
			}
			w.mu.Lock()
			w.backendDone = true
			w.mu.Unlock()
			w.closeErr = SafeError(w.raw.Close())
			// SDK 超时可先退出处理循环，用户函数仍可能没有响应取消。持续跟踪真实函数。
			w.handlersWG.Wait()
			close(w.done)
		}()
	})
}

// Shutdown 开始停止取任务并等待退出。调用方期限限制等待；不打断 SDK 的重新入队收尾。
// SDK 在配置 ShutdownTimeout 后取消未完成任务并尝试重新入队；不响应 Context 的函数无法强行终止。
// 超时返回不代表后台收尾已结束；之后可再次调用等待。同一个 Worker 只启动一次。
func (w *Worker) Shutdown(ctx context.Context) error {
	if ctx == nil {
		return errors.New("taskqueue shutdown context must not be nil")
	}
	w.beginShutdown()
	work, cancel := context.WithTimeout(ctx, w.options.ShutdownTimeout)
	defer cancel()
	select {
	case <-w.done:
		return w.closeErr
	default:
	}
	select {
	case <-w.done:
		return w.closeErr
	case <-work.Done():
		return SafeError(work.Err())
	}
}

// Close 与 Shutdown 相同，采用组件配置的等待上限。
func (w *Worker) Close() error     { return w.Shutdown(context.Background()) }
func (w *Worker) String() string   { return w.options.String() }
func (w *Worker) GoString() string { return w.String() }
