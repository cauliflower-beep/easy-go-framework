/**
 * @Author: LiuShuXin
 * @Description: 验证 Redis 排空、强制关闭并发边界及按次回调期限
 * @File: lifecycle_test.go
 * Software: Goland
 * @Date: 2026/10/14
 */
package redis

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

func waitForDrain(t *testing.T, c *Client) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		closing := c.closing
		c.mu.Unlock()
		if closing {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("shutdown did not close admission")
}

func awaitOperation(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("operation did not finish")
		return nil
	}
}

func TestShutdownDrainsAdmittedCallback(t *testing.T) {
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var first sync.Once
	o := replyFixture(t, func(conn net.Conn, _ []string) {
		first.Do(func() { entered <- struct{}{}; <-release })
		io.WriteString(conn, "+OK\r\n")
	})
	o.CommandTimeout, o.ReadTimeout = 3*time.Second, 3*time.Second
	o.ShutdownTimeout = 2 * time.Second
	c := fixtureClient(t, o, zap.NewNop())
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	operation := make(chan error, 1)
	go func() {
		operation <- c.WithClient(context.Background(), func(ctx context.Context, cmd goredis.Cmdable) error {
			if err := cmd.Get(ctx, "private-first").Err(); err != nil {
				return err
			}
			// 停止准入后，原回调仍可执行后续命令，且 Context 不被提前取消。
			return cmd.Get(ctx, "private-second").Err()
		})
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("command did not reach fixture")
	}
	shutdowns := make(chan error, 8)
	for range 8 {
		go func() { shutdowns <- c.Shutdown(context.Background()) }()
	}
	waitForDrain(t, c)
	if err := c.Ping(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("draining admitted Ping: %v", err)
	}
	if err := c.WithClientTimeout(context.Background(), time.Second, func(context.Context, goredis.Cmdable) error {
		t.Error("draining admitted callback")
		return nil
	}); !errors.Is(err, ErrClosed) {
		t.Fatalf("draining did not reject callback: %v", err)
	}
	select {
	case err := <-shutdowns:
		t.Fatalf("shutdown finished before admitted work: %v", err)
	default:
	}
	unblock()
	if err := awaitOperation(t, operation); err != nil {
		t.Fatalf("graceful shutdown interrupted admitted work: %v", err)
	}
	for range 8 {
		if err := awaitOperation(t, shutdowns); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Shutdown(ctx); err != nil {
		t.Fatalf("completed shutdown was not idempotent: %v", err)
	}
}

func TestShutdownDeadlineAndCancellationForceIO(t *testing.T) {
	for _, name := range []string{"configured limit", "caller deadline", "caller cancellation", "concurrent Close"} {
		t.Run(name, func(t *testing.T) {
			entered := make(chan struct{}, 1)
			o := replyFixture(t, func(net.Conn, []string) { entered <- struct{}{} })
			o.CommandTimeout, o.ReadTimeout = 5*time.Second, 5*time.Second
			o.ShutdownTimeout = time.Second
			if name == "configured limit" {
				o.ShutdownTimeout = 40 * time.Millisecond
			}
			c := fixtureClient(t, o, zap.NewNop())
			operation := make(chan error, 1)
			go func() {
				operation <- c.WithClient(context.Background(), func(ctx context.Context, cmd goredis.Cmdable) error {
					return cmd.Get(ctx, "private-blocked").Err()
				})
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("command did not reach fixture")
			}
			ctx, cancel := context.WithCancel(context.Background())
			if name == "caller deadline" {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 40*time.Millisecond)
			}
			defer cancel()
			shutdown := make(chan error, 1)
			start := time.Now()
			go func() { shutdown <- c.Shutdown(ctx) }()
			waitForDrain(t, c)
			if name == "caller cancellation" {
				cancel()
			}
			if name == "concurrent Close" {
				if err := c.Close(); err != nil {
					t.Fatal(err)
				}
			}
			err := awaitOperation(t, shutdown)
			switch name {
			case "caller cancellation":
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("shutdown lost cancellation: %v", err)
				}
			case "concurrent Close":
				if err != nil {
					t.Fatal(err)
				}
			default:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("shutdown did not report deadline: %v", err)
				}
			}
			if time.Since(start) > 500*time.Millisecond {
				t.Fatal("shutdown waited for original command timeout")
			}
			if err := awaitOperation(t, operation); !errors.Is(err, context.Canceled) {
				t.Fatalf("force-close left IO running or lost cancellation: %v", err)
			}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestShutdownBoundsNonCooperativeCallback(t *testing.T) {
	o := replyFixture(t, func(net.Conn, []string) {})
	o.ShutdownTimeout = 30 * time.Millisecond
	c := fixtureClient(t, o, zap.NewNop())
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	operation := make(chan error, 1)
	go func() {
		operation <- c.WithClient(context.Background(), func(context.Context, goredis.Cmdable) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	if err := c.Shutdown(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("uncooperative callback prevented bounded shutdown: %v", err)
	}
	select {
	case <-c.closed:
		t.Fatal("reported callback cleanup complete before callback returned")
	default:
	}
	releaseOnce.Do(func() { close(release) })
	if err := awaitOperation(t, operation); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWithClientTimeoutOverridesOnlyCallbackBudget(t *testing.T) {
	o := replyFixture(t, func(conn net.Conn, _ []string) {
		time.Sleep(80 * time.Millisecond)
		io.WriteString(conn, "+OK\r\n")
	})
	o.CommandTimeout = 30 * time.Millisecond
	c := fixtureClient(t, o, zap.NewNop())
	get := func(ctx context.Context, cmd goredis.Cmdable) error { return cmd.Get(ctx, "private").Err() }
	if err := c.WithClientTimeout(context.Background(), 500*time.Millisecond, get); err != nil {
		t.Fatalf("explicit budget did not extend default: %v", err)
	}
	if err := c.WithClient(context.Background(), get); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("override changed default budget: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := c.WithClientTimeout(ctx, time.Second, get); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("override extended caller deadline: %v", err)
	}
	for _, timeout := range []time.Duration{0, -time.Second} {
		if err := c.WithClientTimeout(context.Background(), timeout, func(context.Context, goredis.Cmdable) error {
			t.Error("invalid timeout admitted callback")
			return nil
		}); err == nil {
			t.Fatal("invalid timeout accepted")
		}
	}
	if err := c.WithClientTimeout(nil, time.Second, get); err == nil {
		t.Fatal("nil context accepted")
	}
	if err := c.WithClientTimeout(context.Background(), time.Second, nil); err == nil {
		t.Fatal("nil callback accepted")
	}
	if err := c.Shutdown(nil); err == nil {
		t.Fatal("nil shutdown context accepted")
	}
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("invalid shutdown closed client: %v", err)
	}
}

func TestWithClientTimeoutPreservesReadTimeout(t *testing.T) {
	o := replyFixture(t, func(net.Conn, []string) {})
	o.ReadTimeout = 80 * time.Millisecond
	c := fixtureClient(t, o, zap.NewNop())
	start := time.Now()
	err := c.WithClientTimeout(context.Background(), time.Second, func(ctx context.Context, cmd goredis.Cmdable) error {
		return cmd.Get(ctx, "private").Err()
	})
	var network net.Error
	if !errors.As(err, &network) || !network.Timeout() || errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("override disabled ordinary network timeout: %v", err)
	}
}
