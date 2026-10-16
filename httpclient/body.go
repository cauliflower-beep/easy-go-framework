/**
 * @Author: LiuShuXin
 * @Description: 流式响应的有界读取、取消与资源回收
 * @File: body.go
 * Software: Goland
 * @Date: 2026/10/14
 */
package httpclient

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

type operation struct {
	client         *Client
	ctx            context.Context
	cancel         context.CancelFunc
	stopCancel     func() bool
	started        time.Time
	headerDuration time.Duration
	method         string
	scheme         string
	host           string
	status         int
	mu             sync.Mutex
	body           *responseBody
	finishOnce     sync.Once
}

func (op *operation) finish(err error, complete bool, bytes int64) {
	op.finishOnce.Do(func() {
		op.stopCancel()
		op.cancel()
		// 排空完成包含本组件日志收尾；应用可在 Shutdown 成功后关闭 logger。
		defer op.client.release(op)
		op.logOutcome(err, complete, bytes)
	})
}

type responseBody struct {
	raw       io.ReadCloser
	op        *operation
	limit     int64
	bytes     atomic.Int64
	readMu    sync.Mutex
	mu        sync.Mutex
	terminal  error
	closeOnce sync.Once
	closeErr  error
}

func (b *responseBody) Read(p []byte) (int, error) {
	b.readMu.Lock()
	defer b.readMu.Unlock()
	b.mu.Lock()
	terminal := b.terminal
	b.mu.Unlock()
	if terminal != nil {
		return 0, terminal
	}
	if len(p) == 0 {
		return 0, nil
	}
	if err := b.op.ctx.Err(); err != nil {
		b.terminate(err, false)
		return 0, SafeError(err)
	}
	if b.limit > 0 {
		remaining := b.limit - b.bytes.Load()
		if remaining == 0 {
			// 只探测一个额外字节，以区分恰好达到限制与真正超限；不缓存全部响应。
			var probe [1]byte
			n, err := b.raw.Read(probe[:])
			if n > 0 {
				b.terminate(ErrResponseTooLarge, false)
				return 0, SafeError(ErrResponseTooLarge)
			}
			return b.readResult(0, err)
		}
		if int64(len(p)) > remaining {
			p = p[:int(remaining)]
		}
	}
	n, err := b.raw.Read(p)
	b.bytes.Add(int64(n))
	return b.readResult(n, err)
}

func (b *responseBody) readResult(n int, err error) (int, error) {
	if canceled := b.op.ctx.Err(); canceled != nil {
		err = errors.Join(err, canceled)
	}
	if err == io.EOF {
		b.terminate(nil, true)
		// io.Reader 的 EOF 必须保留直接相等语义，兼容 io.ReadAll 等标准调用。
		return n, io.EOF
	}
	if err != nil {
		b.terminate(err, false)
		return n, SafeError(err)
	}
	return n, nil
}

// terminate 的状态锁不跨越网络读取；Close 可以中断正在阻塞的 Read。
func (b *responseBody) terminate(cause error, complete bool) {
	b.closeOnce.Do(func() {
		b.mu.Lock()
		switch {
		case complete:
			b.terminal = io.EOF
		case cause != nil:
			b.terminal = SafeError(cause)
		default:
			b.terminal = SafeError(ErrBodyClosed)
		}
		b.mu.Unlock()
		b.closeErr = SafeError(b.raw.Close())
		b.op.finish(errors.Join(cause, b.closeErr), complete, b.bytes.Load())
	})
}

func (b *responseBody) Close() error {
	b.terminate(nil, false)
	return b.closeErr
}
