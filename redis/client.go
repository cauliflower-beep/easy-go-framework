/**
 * @Author: LiuShuXin
 * @Description: Redis 启动校验、共享客户端作用域与有界资源回收
 * @File: client.go
 * Software: Goland
 * @Date: 2026/10/14
 */
package redis

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// Client 拥有唯一命令连接池。客户端、Pipeline 不得带出 WithClient 回调。
// 调用方始终使用回调提供的 Context；回调应返回最终命令/Exec 的 Error。
type Client struct {
	raw        *goredis.Client
	options    Options
	logger     *zap.Logger
	life       context.Context
	cancel     context.CancelFunc
	mu         sync.Mutex
	closing    bool
	operations sync.WaitGroup
	drainOnce  sync.Once
	drained    chan struct{}
	closeOnce  sync.Once
	closed     chan struct{}
	closeErr   error
}

// Open 在调用方取消及 ConnectTimeout 内同步验证连接，不改变 Redis 数据。
func Open(ctx context.Context, options Options, logger *zap.Logger) (*Client, error) {
	if ctx == nil || logger == nil {
		return nil, errors.New("Redis context and logger must not be nil")
	}
	if err := options.Validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, SafeError(err)
	}
	settings, err := clientOptions(options)
	if err != nil {
		return nil, err
	}
	life, cancel := context.WithCancel(context.Background())
	c := &Client{raw: goredis.NewClient(settings), options: options, logger: logger, life: life, cancel: cancel, drained: make(chan struct{}), closed: make(chan struct{})}
	c.raw.AddHook(commandHook{logger: logger, slow: options.SlowThreshold})
	work, stop := context.WithTimeout(ctx, options.ConnectTimeout)
	err = c.raw.Ping(work).Err()
	err = errors.Join(err, work.Err())
	stop()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("connect Redis: %w", SafeError(errors.Join(err, c.raw.Close())))
	}
	logger.Info("Redis connected", zap.String("host", options.Host), zap.Int("port", options.Port), zap.Int("database", options.Database), zap.Int("pool_size", options.PoolSize), zap.Bool("tls", options.TLS))
	return c, nil
}

func (c *Client) scope(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc, error) {
	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		return nil, nil, ErrClosed
	}
	c.operations.Add(1)
	c.mu.Unlock()
	work, cancel := context.WithTimeout(ctx, timeout)
	stop := context.AfterFunc(c.life, cancel)
	if c.life.Err() != nil {
		cancel()
	}
	return work, func() { stop(); cancel(); c.operations.Done() }, nil
}

// WithClient 复用 go-redis 的 Cmdable，不重新包装命令 API 或承诺跨存储事务。
// CommandTimeout 是整个回调的总期限，包含池等待、握手及回调内全部命令。
// 已开始的网络 I/O 按设置好的最短期限退出；取消不证明写命令尚未执行。
func (c *Client) WithClient(ctx context.Context, fn func(context.Context, goredis.Cmdable) error) error {
	return c.WithClientTimeout(ctx, c.options.CommandTimeout, fn)
}

// WithClientTimeout 为本次回调指定正的总期限，可长于默认 CommandTimeout，
// 但不延长调用方 Context 的期限或普通命令的 Read/WriteTimeout。
// 阻塞命令采用驱动按命令设置的读取期限；长期阻塞应使用独立客户端/连接池。
func (c *Client) WithClientTimeout(ctx context.Context, timeout time.Duration, fn func(context.Context, goredis.Cmdable) error) error {
	if ctx == nil || fn == nil {
		return errors.New("Redis context and callback must not be nil")
	}
	if timeout <= 0 {
		return errors.New("Redis callback timeout must be positive")
	}
	work, cancel, err := c.scope(ctx, timeout)
	if err != nil {
		return SafeError(err)
	}
	defer cancel()
	if err := work.Err(); err != nil {
		return SafeError(err)
	}
	err = fn(work, c.raw)
	// AfterFunc 异步通知回调 Context；直接读取 life 的状态，避免强制关闭
	// 与用户回调同时返回时遗漏取消。优雅关闭只在所有回调结束后取消 life。
	return SafeError(errors.Join(commandError(work, err), work.Err(), c.life.Err()))
}

// Ping 使用独立健康检查期限，但仍遵守调用方更短的期限与取消。
func (c *Client) Ping(ctx context.Context) error {
	if ctx == nil {
		return errors.New("Redis context must not be nil")
	}
	work, cancel, err := c.scope(ctx, c.options.HealthTimeout)
	if err != nil {
		return SafeError(err)
	}
	defer cancel()
	if err := work.Err(); err != nil {
		return SafeError(err)
	}
	return SafeError(errors.Join(c.raw.Ping(work).Err(), work.Err(), c.life.Err()))
}

// Stats 返回值快照，调用方无法修改池状态。
func (c *Client) Stats() goredis.PoolStats { return *c.raw.PoolStats() }

// beginDrain 先关闭准入，再启动唯一等待者，避免 Wait 与新的 Add 并发。
// 排空期间不取消已准入的回调，它们仍可以继续执行命令。
func (c *Client) beginDrain() {
	c.drainOnce.Do(func() {
		c.mu.Lock()
		c.closing = true
		c.mu.Unlock()
		go func() { c.operations.Wait(); close(c.drained) }()
	})
}

func (c *Client) closeResources() {
	c.closeOnce.Do(func() {
		c.cancel()
		go func() { c.closeErr = c.raw.Close(); <-c.drained; close(c.closed) }()
	})
}

func (c *Client) waitClosed(ctx context.Context) error {
	// 已完成的关闭保持幂等，即使重复调用时传入的 Context 已取消。
	select {
	case <-c.closed:
		return SafeError(c.closeErr)
	default:
	}
	select {
	case <-c.closed:
		return SafeError(c.closeErr)
	case <-ctx.Done():
		return SafeError(ctx.Err())
	}
}

// Shutdown 拒绝新回调，等待已准入的回调自然完成，然后释放连接。
// 等待上限取 ctx 期限与 ShutdownTimeout 中较短者；取消或超时后强制关闭，
// 返回相应 Context 错误，未退出的用户回调继续在后台收尾。
// 并发 Close 或任何 Shutdown 的取消/超时都会终止所有调用者的排空等待。
// 在回调外调用；应用先停止请求入口和后台任务，再关闭依赖，最后关闭日志。
func (c *Client) Shutdown(ctx context.Context) error {
	if ctx == nil {
		return errors.New("Redis shutdown context must not be nil")
	}
	work, cancel := context.WithTimeout(ctx, c.options.ShutdownTimeout)
	defer cancel()
	c.beginDrain()
	select {
	case <-c.closed:
		return SafeError(c.closeErr)
	default:
	}
	if err := work.Err(); err != nil {
		c.closeResources()
		return SafeError(err)
	}
	select {
	case <-c.closed:
		return SafeError(c.closeErr)
	case <-c.drained:
		c.closeResources()
		return c.waitClosed(work)
	case <-work.Done():
		c.closeResources()
		return SafeError(work.Err())
	}
}

// Close 立即拒绝新回调、取消并关闭套接字，再在 ShutdownTimeout 内等待收尾。
// 正常退出使用 Shutdown；Close 用于强制关闭或已排空后的资源回收。
// 不响应取消的用户回调仍可能超出等待期限。
func (c *Client) Close() error {
	work, cancel := context.WithTimeout(context.Background(), c.options.ShutdownTimeout)
	defer cancel()
	c.beginDrain()
	c.closeResources()
	return c.waitClosed(work)
}

func (c *Client) String() string   { return c.options.String() }
func (c *Client) GoString() string { return c.String() }
