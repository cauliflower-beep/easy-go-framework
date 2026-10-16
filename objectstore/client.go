/**
 * @Author: LiuShuXin
 * @Description: 对象操作统一入口、流式下载及有界关闭
 * @File: client.go
 * Software: Goland
 * @Date: 2026/10/15
 */
package objectstore

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/cauliflower-beep/easy-go-framework/httpclient"
	"github.com/cauliflower-beep/easy-go-framework/logging"
	"go.uber.org/zap"
)

type driver interface {
	put(context.Context, PutInput, int64) (ObjectInfo, error)
	get(context.Context, string) (*Object, error)
	stat(context.Context, string) (ObjectInfo, error)
	delete(context.Context, string) error
	ping(context.Context) error
	sign(context.Context, string, string, time.Duration) (SignedRequest, error)
}

// Client 并发安全，持有一个 bucket 及独立连接池，不自动创建或修改 bucket。
type Client struct {
	options     Options
	driver      driver
	signer      driver
	http        *httpclient.Client
	logger      *zap.Logger
	mu          sync.Mutex
	closing     bool
	active      map[*call]struct{}
	drained     chan struct{}
	drainedOnce sync.Once
	forceOnce   sync.Once
}

// New 仅校验并装配。需要网络验收时显式调用 Ping；logger 不得为 nil。
func New(o Options, logger *zap.Logger, middlewares ...httpclient.Middleware) (*Client, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		return nil, errors.New("objectstore logger must not be nil")
	}
	// 对象的 Content-Encoding 可能描述已存储的 gzip 数据。禁用 HTTP 自动解压，
	// 保证 Get 的字节与对象本身相同；否则下载会被透明改写。
	chain := append(append([]httpclient.Middleware(nil), middlewares...), preserveObjectBytes)
	h, err := httpclient.New(o.HTTP, logger.Named("http"), chain...)
	if err != nil {
		return nil, err
	}
	c := &Client{options: o, http: h, logger: logger, active: make(map[*call]struct{}), drained: make(chan struct{})}
	c.driver = newDriver(o, h)
	c.signer = c.driver
	if o.PublicEndpoint != "" && o.PublicEndpoint != o.Endpoint {
		o.Endpoint = o.PublicEndpoint
		c.signer = newDriver(o, h)
	}
	return c, nil
}

func preserveObjectBytes(next http.RoundTripper) http.RoundTripper {
	return httpclient.RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		clone := req.Clone(req.Context())
		clone.Header.Set("Accept-Encoding", "identity")
		return next.RoundTrip(clone)
	})
}

type call struct {
	client  *Client
	ctx     context.Context
	cancel  context.CancelFunc
	once    sync.Once
	name    string
	started time.Time
}

func (c *Client) begin(ctx context.Context, name string) (*call, error) {
	if ctx == nil {
		return nil, errors.New("objectstore context must not be nil")
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
	op := &call{client: c, ctx: work, cancel: cancel, name: name, started: time.Now()}
	c.active[op] = struct{}{}
	return op, nil
}
func (op *call) finish(err error) {
	op.once.Do(func() {
		op.cancel()
		defer func() { c := op.client; c.mu.Lock(); defer c.mu.Unlock(); delete(c.active, op); c.signalDrained() }()
		if err != nil {
			fields := []zap.Field{zap.String("operation", op.name), zap.String("provider", op.client.options.Provider), zap.Duration("duration", time.Since(op.started))}
			fields = append(fields, logging.ErrorFields(SafeError(err))...)
			logging.ForRequest(op.client.logger, op.ctx).Warn("objectstore operation failed", fields...)
		}
	})
}

// Put 上传剩余正文；SDK 不负责关闭调用方的 Body。写入失败可能结果未知。
func (c *Client) Put(ctx context.Context, in PutInput) (out ObjectInfo, err error) {
	op, err := c.begin(ctx, "PUT")
	if err != nil {
		return out, err
	}
	defer func() { op.finish(err) }()
	size, err := validatePut(in)
	if err != nil {
		return out, err
	}
	in.Metadata = cloneMetadata(in.Metadata)
	out, err = c.driver.put(op.ctx, in, size)
	err = SafeError(err)
	return
}

// Get 返回流式正文，总期限覆盖调用方消费时间；读取到 EOF 或 Close 才完成操作。
func (c *Client) Get(ctx context.Context, key string) (out *Object, err error) {
	op, err := c.begin(ctx, "GET")
	if err != nil {
		return nil, err
	}
	handedOff := false
	defer func() {
		if !handedOff {
			op.finish(err)
		}
	}()
	if err = validKey(key); err != nil {
		return nil, err
	}
	out, err = c.driver.get(op.ctx, key)
	if err != nil {
		return nil, SafeError(err)
	}
	b := &objectBody{raw: out.Body, op: op}
	// 取消可与 SDK 交付正文竞争；AfterFunc 只能在 b 全部字段就绪后启动。
	b.stop = context.AfterFunc(op.ctx, func() { b.end(op.ctx.Err()) })
	out.Body = b
	handedOff = true
	return out, nil
}

func (c *Client) Stat(ctx context.Context, key string) (out ObjectInfo, err error) {
	op, err := c.begin(ctx, "HEAD")
	if err != nil {
		return out, err
	}
	defer func() { op.finish(err) }()
	if err = validKey(key); err != nil {
		return out, err
	}
	out, err = c.driver.stat(op.ctx, key)
	err = SafeError(err)
	return
}

// Delete 遵循存储服务幂等删除语义；不提供“仅存在才删除”的额外检查。
func (c *Client) Delete(ctx context.Context, key string) (err error) {
	op, err := c.begin(ctx, "DELETE")
	if err != nil {
		return err
	}
	defer func() { op.finish(err) }()
	if err = validKey(key); err != nil {
		return err
	}
	return SafeError(c.driver.delete(op.ctx, key))
}

// Ping 以 S3 HeadBucket / OSS GetBucketInfo 验证访问，需要对应 bucket 权限。
// 成功不等于读写权限均已验证。
func (c *Client) Ping(ctx context.Context) (err error) {
	op, err := c.begin(ctx, "HEAD_BUCKET")
	if err != nil {
		return err
	}
	defer func() { op.finish(err) }()
	return SafeError(c.driver.ping(op.ctx))
}

func (c *Client) PresignGet(ctx context.Context, key string, expires time.Duration) (SignedRequest, error) {
	return c.presign(ctx, "GET", key, expires)
}
func (c *Client) PresignPut(ctx context.Context, key string, expires time.Duration) (SignedRequest, error) {
	return c.presign(ctx, "PUT", key, expires)
}
func (c *Client) presign(ctx context.Context, method, key string, expires time.Duration) (out SignedRequest, err error) {
	op, err := c.begin(ctx, "PRESIGN_"+method)
	if err != nil {
		return out, err
	}
	defer func() { op.finish(err) }()
	if err = validKey(key); err != nil {
		return out, err
	}
	if expires < time.Second || expires > 7*24*time.Hour || expires%time.Second != 0 {
		return out, errors.New("objectstore signature duration must be whole seconds between 1 second and 7 days")
	}
	out, err = c.signer.sign(op.ctx, method, key, expires)
	err = SafeError(err)
	return
}

func (c *Client) signalDrained() {
	if c.closing && len(c.active) == 0 {
		c.drainedOnce.Do(func() { close(c.drained) })
	}
}
func (c *Client) beginDrain() { c.mu.Lock(); defer c.mu.Unlock(); c.closing = true; c.signalDrained() }
func (c *Client) force() {
	c.forceOnce.Do(func() {
		c.mu.Lock()
		for op := range c.active {
			op.cancel()
		}
		c.mu.Unlock()
		go func() { _ = c.http.Close() }()
	})
}

// Shutdown 停止新操作并排空，最多等待 HTTP.ShutdownTimeout；必须在下载消费流程外调用。
func (c *Client) Shutdown(ctx context.Context) error {
	if ctx == nil {
		return errors.New("objectstore shutdown context must not be nil")
	}
	work, cancel := context.WithTimeout(ctx, c.options.HTTP.ShutdownTimeout)
	defer cancel()
	c.beginDrain()
	select {
	case <-c.drained:
		return c.http.Shutdown(work)
	default:
	}
	select {
	case <-c.drained:
		return c.http.Shutdown(work)
	case <-work.Done():
		c.force()
		return SafeError(work.Err())
	}
}

// Close 立即取消操作，再限时等待；不响应 Context 的调用方 Reader 无法被强行终止。
func (c *Client) Close() error     { c.beginDrain(); c.force(); return c.Shutdown(context.Background()) }
func (c *Client) String() string   { return c.options.String() }
func (c *Client) GoString() string { return c.String() }

type objectBody struct {
	raw      io.ReadCloser
	op       *call
	stop     func() bool
	once     sync.Once
	mu       sync.Mutex
	terminal error
	closeErr error
}

func (b *objectBody) end(err error) {
	b.once.Do(func() {
		b.mu.Lock()
		if err == io.EOF {
			b.terminal = io.EOF
		} else if err != nil {
			b.terminal = SafeError(err)
		} else {
			b.terminal = SafeError(ErrBodyClosed)
		}
		b.mu.Unlock()
		closeErr := b.raw.Close()
		b.mu.Lock()
		b.closeErr = SafeError(closeErr)
		b.mu.Unlock()
		if err == io.EOF {
			err = nil
		}
		b.op.finish(errors.Join(err, closeErr))
	})
}
func (b *objectBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	terminal := b.terminal
	b.mu.Unlock()
	if terminal != nil {
		return 0, terminal
	}
	n, err := b.raw.Read(p)
	if canceled := b.op.ctx.Err(); canceled != nil {
		err = errors.Join(err, canceled)
	}
	if err != nil {
		b.end(err)
	}
	if err == io.EOF {
		return n, io.EOF
	}
	return n, SafeError(err)
}
func (b *objectBody) Close() error {
	b.end(nil)
	b.stop()
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closeErr
}
