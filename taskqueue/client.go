/**
 * @Author: LiuShuXin
 * @Description: 持久化任务提交、带 Context 的状态查询与提交端关闭
 * @File: client.go
 * Software: Goland
 * @Date: 2026/10/15
 */
package taskqueue

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cauliflower-beep/easy-go-framework/logging"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	goredis "github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// Client 可独立用于 API 进程，不会启动消费者或修改 Redis 持久化设置。
type Client struct {
	options     Options
	raw         *goredis.Client
	backend     *asynq.Client
	logger      *zap.Logger
	mu          sync.Mutex
	closing     bool
	active      map[*request]struct{}
	drained     chan struct{}
	drainedOnce sync.Once
	closeOnce   sync.Once
	closed      chan struct{}
	closeErr    error
}
type request struct {
	client  *Client
	ctx     context.Context
	cancel  context.CancelFunc
	name    string
	started time.Time
}

func NewClient(o Options, logger *zap.Logger) (*Client, error) {
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
	raw := goredis.NewClient(settings)
	return &Client{options: o, raw: raw, backend: asynq.NewClientFromRedisClient(raw), logger: logger, active: make(map[*request]struct{}), drained: make(chan struct{}), closed: make(chan struct{})}, nil
}

func (c *Client) begin(ctx context.Context, name string) (*request, error) {
	if ctx == nil {
		return nil, errors.New("taskqueue context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, SafeError(err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closing {
		return nil, SafeError(ErrClosed)
	}
	work, cancel := context.WithTimeout(ctx, c.options.OperationTimeout)
	op := &request{client: c, ctx: work, cancel: cancel, name: name, started: time.Now()}
	c.active[op] = struct{}{}
	return op, nil
}
func (op *request) finish(err error) {
	op.cancel()
	defer func() { c := op.client; c.mu.Lock(); defer c.mu.Unlock(); delete(c.active, op); c.signalDrained() }()
	if err != nil {
		fields := []zap.Field{zap.String("operation", op.name), zap.Duration("duration", time.Since(op.started))}
		fields = append(fields, logging.ErrorFields(SafeError(err))...)
		logging.ForRequest(op.client.logger, op.ctx).Warn("taskqueue operation failed", fields...)
	}
}

// Enqueue 只确认 Redis 已接受任务；超时可能结果未知，可用预先指定的 ID 核实。
// 重用 ID 在任务仍保留时返回 ErrIDConflict，不代表原任务已成功或只执行一次。
func (c *Client) Enqueue(ctx context.Context, in Submission) (out Info, err error) {
	op, err := c.begin(ctx, "ENQUEUE")
	if err != nil {
		return out, err
	}
	defer func() { op.finish(err) }()
	if err = c.ValidateSubmission(in); err != nil {
		return out, err
	}
	if in.ID == "" {
		id, generateErr := uuid.NewRandom()
		if generateErr != nil {
			return out, SafeError(generateErr)
		}
		in.ID = id.String()
	}
	if in.Timeout == 0 {
		in.Timeout = c.options.TaskTimeout
	}
	if len(in.Payload) == 0 {
		in.Payload = json.RawMessage("null")
	}
	requestID := logging.RequestID(ctx)
	if !validCorrelation(requestID) {
		requestID = ""
	}
	payload, err := json.Marshal(envelope{Version: 1, RequestID: requestID, Data: in.Payload})
	if err != nil {
		return out, SafeError(err)
	}
	opts := []asynq.Option{asynq.TaskID(in.ID), asynq.Queue(c.options.Queue), asynq.Timeout(in.Timeout), asynq.Retention(c.options.Retention), asynq.MaxRetry(in.MaxRetry)}
	if in.Delay > 0 {
		// SDK 的延迟队列按 Unix 秒保存。向上取整，避免任务早于指定延迟执行。
		due := time.Now().Add(in.Delay)
		if due.Nanosecond() != 0 {
			due = due.Truncate(time.Second).Add(time.Second)
		}
		opts = append(opts, asynq.ProcessAt(due))
	}
	info, err := c.backend.EnqueueContext(op.ctx, asynq.NewTask(in.Type, payload), opts...)
	if err != nil {
		return out, SafeError(errors.Join(err, op.ctx.Err()))
	}
	return snapshot(info), nil
}

// ValidateSubmission 仅验证提交数据，不连接 Redis、分配 ID 或检查客户端启停状态。
// 可供调度器在启动前使用与实际提交一致的类型、大小及期限约束。
func (c *Client) ValidateSubmission(in Submission) error {
	if !namePattern.MatchString(in.Type) {
		return errors.New("taskqueue task type is invalid")
	}
	if in.ID != "" && !validID(in.ID) {
		return errors.New("taskqueue task ID must be a canonical nonzero UUID")
	}
	if in.Delay < 0 || in.Delay%time.Second != 0 || in.Timeout < 0 || in.Timeout%time.Second != 0 || in.MaxRetry < 0 {
		return errors.New("taskqueue delay and timeout require nonnegative whole seconds; max retry must not be negative")
	}
	payload := in.Payload
	if len(payload) == 0 {
		payload = json.RawMessage("null")
	}
	if len(payload) > c.options.MaxPayloadBytes {
		return errors.New("taskqueue payload exceeds limit")
	}
	if !json.Valid(payload) || !utf8.Valid(payload) {
		return errors.New("taskqueue payload must be valid JSON")
	}
	return nil
}

// Get 不返回原始 Payload 或任意异常文本；调用方仍须检查任务访问权限后再公开结果。
func (c *Client) Get(ctx context.Context, id string) (out Info, err error) {
	op, err := c.begin(ctx, "GET")
	if err != nil {
		return out, err
	}
	defer func() { op.finish(err) }()
	if !validID(id) {
		return out, errors.New("taskqueue task ID must be a canonical nonzero UUID")
	}
	// Asynq Inspector 的接口不接受 Context。适配其公开 Redis 命令接口，
	// 让队列检查和 Lua 查询共同遵守同一请求期限，不另建连接池或读取私有键。
	inspector := asynq.NewInspectorFromRedisClient(boundRedis{UniversalClient: c.raw, ctx: op.ctx})
	info, err := inspector.GetTaskInfo(c.options.Queue, id)
	if err != nil {
		return out, SafeError(errors.Join(err, op.ctx.Err()))
	}
	if len(info.Result) > c.options.MaxResultBytes {
		return out, SafeError(errors.New("task result exceeds limit"))
	}
	return snapshot(info), nil
}

func (c *Client) Ping(ctx context.Context) (err error) {
	op, err := c.begin(ctx, "PING")
	if err != nil {
		return err
	}
	defer func() { op.finish(err) }()
	return SafeError(errors.Join(c.raw.Ping(op.ctx).Err(), op.ctx.Err()))
}

func (c *Client) signalDrained() {
	if c.closing && len(c.active) == 0 {
		c.drainedOnce.Do(func() { close(c.drained) })
	}
}
func (c *Client) beginDrain() { c.mu.Lock(); defer c.mu.Unlock(); c.closing = true; c.signalDrained() }
func (c *Client) closeResources() {
	c.closeOnce.Do(func() { go func() { c.closeErr = SafeError(c.raw.Close()); <-c.drained; close(c.closed) }() })
}
func (c *Client) force() {
	c.mu.Lock()
	for op := range c.active {
		op.cancel()
	}
	c.mu.Unlock()
	c.closeResources()
}
func (c *Client) wait(ctx context.Context) error {
	select {
	case <-c.closed:
		return c.closeErr
	default:
	}
	select {
	case <-c.closed:
		return c.closeErr
	case <-ctx.Done():
		return SafeError(ctx.Err())
	}
}
func (c *Client) Shutdown(ctx context.Context) error {
	if ctx == nil {
		return errors.New("taskqueue shutdown context must not be nil")
	}
	work, cancel := context.WithTimeout(ctx, c.options.ShutdownTimeout)
	defer cancel()
	c.beginDrain()
	select {
	case <-c.drained:
		c.closeResources()
		return c.wait(work)
	case <-work.Done():
		c.force()
		return SafeError(work.Err())
	}
}
func (c *Client) Close() error {
	c.beginDrain()
	c.force()
	work, cancel := context.WithTimeout(context.Background(), c.options.ShutdownTimeout)
	defer cancel()
	return c.wait(work)
}
func (c *Client) String() string   { return c.options.String() }
func (c *Client) GoString() string { return c.String() }
