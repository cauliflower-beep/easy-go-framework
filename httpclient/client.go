/**
 * @Author: LiuShuXin
 * @Description: 标准 HTTP 请求接口、按次期限与响应体生命周期管理
 * @File: client.go
 * Software: Goland
 * @Date: 2026/10/14
 */
package httpclient

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Client 拥有独立 Transport，可并发使用并作为 SDK 的 Do 接口实现注入。
// 调用方须读取响应体至 EOF 或关闭它；不得修改正在使用的请求。
type Client struct {
	raw       *http.Client
	transport *http.Transport
	options   Options
	logger    *zap.Logger
	mu        sync.Mutex
	closing   bool
	active    map[*operation]struct{}
	drained   chan struct{}
	drainDone bool
	closeOnce sync.Once
	closed    chan struct{}
}

// New 构造本地客户端，不探测任何外部地址或修改默认客户端/Transport。
// 可选中间件按参数顺序由外到内执行，构造后不再改变调用链。
func New(options Options, logger *zap.Logger, middlewares ...Middleware) (*Client, error) {
	if logger == nil {
		return nil, errors.New("HTTP client logger must not be nil")
	}
	if err := options.Validate(); err != nil {
		return nil, err
	}
	transport, err := newTransport(options)
	if err != nil {
		return nil, err
	}
	chain, err := chainTransport(transport, middlewares)
	if err != nil {
		transport.CloseIdleConnections()
		return nil, err
	}
	c := &Client{
		transport: transport, options: options, logger: logger,
		active: make(map[*operation]struct{}), drained: make(chan struct{}), closed: make(chan struct{}),
	}
	// 总期限由请求 Context 管理，并延续至响应体结束；不使用共享 Client.Timeout
	// 限制所有流式请求，亦不暴露可修改生命周期/Transport 的原始 Client。
	c.raw = &http.Client{Transport: chain, CheckRedirect: c.checkRedirect}
	return c, nil
}

// Do 保留标准 HTTP 状态语义：4xx/5xx 仍是响应，不转成业务错误。
// 请求总期限及大小限制使用客户端默认值，响应体归调用方消费。
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	return c.DoWithOptions(req, RequestOptions{})
}

// DoWithOptions 按次覆盖期限和响应体限制，仍遵守调用方更短的 Context。
// 不额外重试；标准 Transport 对可重放请求的有限网络恢复行为仍然有效。
// 超时、取消和断连均不能证明外部操作未执行。
func (c *Client) DoWithOptions(req *http.Request, options RequestOptions) (*http.Response, error) {
	if req == nil {
		return nil, errors.New("HTTP request must not be nil")
	}
	reject := func(err error) (*http.Response, error) {
		// 与标准 Client 保持相同的请求体所有权：交给 Do 后，即使失败也会关闭。
		if req.Body != nil {
			err = errors.Join(err, req.Body.Close())
		}
		return nil, SafeError(err)
	}
	if req.URL == nil || (req.URL.Scheme != "http" && req.URL.Scheme != "https") || req.URL.Host == "" || req.URL.Opaque != "" || req.RequestURI != "" {
		return reject(errors.New("HTTP request must have an absolute HTTP(S) URL and no RequestURI"))
	}
	if options.Timeout < 0 || options.MaxResponseBytes < -1 {
		return reject(errors.New("HTTP request timeout must not be negative; response limit must be nonnegative or -1"))
	}
	if err := req.Context().Err(); err != nil {
		return reject(err)
	}
	timeout, limit := c.options.RequestTimeout, c.options.MaxResponseBytes
	if options.Timeout > 0 {
		timeout = options.Timeout
	}
	if options.MaxResponseBytes != 0 {
		limit = options.MaxResponseBytes
	}
	work, cancel := context.WithTimeout(req.Context(), timeout)
	op := &operation{client: c, ctx: work, cancel: cancel, started: time.Now(), method: safeMethod(req.Method), scheme: req.URL.Scheme, host: safeHost(req.URL)}
	op.stopCancel = context.AfterFunc(work, func() {
		op.mu.Lock()
		body := op.body
		op.mu.Unlock()
		if body != nil {
			body.terminate(work.Err(), false)
		}
	})
	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		op.stopCancel()
		cancel()
		return reject(ErrClosed)
	}
	c.active[op] = struct{}{}
	c.mu.Unlock()
	handedOff := false
	defer func() {
		// GetBody 等调用方提供的函数可能 panic。保留 panic，同时撤销登记和
		// 取消底层请求，避免它永远占用排空计数；普通错误路径由 finishOnce 去重。
		if !handedOff {
			op.finish(errors.New("HTTP request aborted before response handoff"), false, 0)
		}
	}()
	// 不修改原请求的 Context、Header、URL；请求体仍遵循标准不可并发复用约定。
	request := req.Clone(work)
	response, err := c.raw.Do(request)
	op.headerDuration = time.Since(op.started)
	if response != nil {
		op.status = response.StatusCode
	}
	if err != nil {
		err = SafeError(errors.Join(err, work.Err()))
		op.finish(err, false, 0)
		// 标准 Client 在重定向被拒绝时可能返回已关闭响应体的 response。
		return response, err
	}
	body := &responseBody{raw: response.Body, op: op, limit: limit}
	op.mu.Lock()
	op.body = body
	op.mu.Unlock()
	handedOff = true
	response.Body = body
	if err := work.Err(); err != nil {
		body.terminate(err, false)
		return nil, SafeError(err)
	}
	if response.StatusCode == http.StatusSwitchingProtocols {
		body.terminate(ErrProtocolUpgrade, false)
		return nil, SafeError(ErrProtocolUpgrade)
	}
	// HEAD/204/304 可携带表示资源大小的 Content-Length，但没有对应响应体。
	if limit > 0 && body.raw != http.NoBody && req.Method != http.MethodHead && response.ContentLength > limit {
		body.terminate(ErrResponseTooLarge, false)
		return nil, SafeError(ErrResponseTooLarge)
	}
	if body.raw == http.NoBody {
		body.terminate(nil, true)
	}
	return response, nil
}

func (c *Client) checkRedirect(req *http.Request, via []*http.Request) error {
	if c.options.MaxRedirects == 0 {
		return http.ErrUseLastResponse
	}
	if len(via) > c.options.MaxRedirects {
		return ErrRedirectLimit
	}
	original := via[0].URL
	if original.Scheme == "https" && req.URL.Scheme != "https" {
		return ErrRedirectDowngrade
	}
	if !sameOrigin(original, req.URL) {
		return ErrRedirectOrigin
	}
	// 自动生成的 Referer 可能包含原路径/查询中的秘密，不随重定向转发。
	req.Header.Del("Referer")
	return nil
}

func sameOrigin(a, b *url.URL) bool {
	port := func(u *url.URL) string {
		if p := u.Port(); p != "" {
			return p
		}
		if u.Scheme == "https" {
			return "443"
		}
		return "80"
	}
	return a.Scheme == b.Scheme && strings.EqualFold(a.Hostname(), b.Hostname()) && port(a) == port(b) && a.User.String() == b.User.String()
}

func (c *Client) release(op *operation) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.active, op)
	c.signalDrained()
}

// signalDrained 在 mu 保护下调用；准入关闭后 active 只会减少。
func (c *Client) signalDrained() {
	if c.closing && len(c.active) == 0 && !c.drainDone {
		c.drainDone = true
		close(c.drained)
	}
}

func (c *Client) beginDrain() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closing = true
	c.signalDrained()
}

func (c *Client) closeResources() {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		pending := make([]*operation, 0, len(c.active))
		for op := range c.active {
			pending = append(pending, op)
		}
		c.mu.Unlock()
		for _, op := range pending {
			// 同步取消每个请求；响应体关闭由其取消回调负责，不依赖异步全局通知。
			op.cancel()
		}
		go func() {
			// 连接关闭及日志收尾不占用调用方的限时等待线程。
			c.transport.CloseIdleConnections()
			<-c.drained
			c.transport.CloseIdleConnections()
			close(c.closed)
		}()
	})
}

func (c *Client) waitClosed(ctx context.Context) error {
	select {
	case <-c.closed:
		return nil
	default:
	}
	select {
	case <-c.closed:
		return nil
	case <-ctx.Done():
		return SafeError(ctx.Err())
	}
}

// Shutdown 拒绝新请求，等待已准入请求的响应体读完/关闭后释放连接。
// 最多等待 ctx 与 ShutdownTimeout 的较短期限，超时或取消后强制关闭。
// 并发 Close 或任一 Shutdown 超时/取消都会终止排空；在响应消费流程外调用。
func (c *Client) Shutdown(ctx context.Context) error {
	if ctx == nil {
		return errors.New("HTTP shutdown context must not be nil")
	}
	work, cancel := context.WithTimeout(ctx, c.options.ShutdownTimeout)
	defer cancel()
	c.beginDrain()
	select {
	case <-c.closed:
		return nil
	default:
	}
	if err := work.Err(); err != nil {
		c.closeResources()
		return SafeError(err)
	}
	select {
	case <-c.closed:
		return nil
	case <-c.drained:
		c.closeResources()
		return c.waitClosed(work)
	case <-work.Done():
		c.closeResources()
		return SafeError(work.Err())
	}
}

// Close 立即取消在途请求并关闭响应体，再在 ShutdownTimeout 内等待资源收尾。
func (c *Client) Close() error {
	work, cancel := context.WithTimeout(context.Background(), c.options.ShutdownTimeout)
	defer cancel()
	c.beginDrain()
	c.closeResources()
	return c.waitClosed(work)
}

// CloseIdleConnections 与标准客户端兼容，仅回收空闲连接，不停止请求准入。
func (c *Client) CloseIdleConnections() { c.transport.CloseIdleConnections() }

func (c *Client) String() string   { return c.options.String() }
func (c *Client) GoString() string { return c.String() }
