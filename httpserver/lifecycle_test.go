/**
 * @Author: LiuShuXin
 * @Description: 验证组件停机期限、强制关闭和真实请求的收尾等待
 * @File: lifecycle_test.go
 * Software: Goland
 * @Date: 2026/10/15
 */
package httpserver

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestConfiguredShutdownDeadline(t *testing.T) {
	settings := testSettings()
	settings.ShutdownTimeout = 30 * time.Millisecond
	s, _ := testServer(t, settings)
	entered, exited := make(chan struct{}), make(chan struct{})
	s.Router().GET("/wait-for-cancel", func(c *gin.Context) {
		defer close(exited)
		close(entered)
		<-c.Request.Context().Done()
	})
	addr, err := s.Start()
	if err != nil {
		t.Fatal(err)
	}
	result := requestAsync("http://" + addr.String() + "/wait-for-cancel")
	waitSignal(t, entered)
	// 安全兜底明显长于组件期限；旧实现会一直等到这个期限。
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	if err := s.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("configured deadline not reported: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("component deadline ignored: %s", elapsed)
	}
	waitSignal(t, exited)
	_ = waitResult(t, result)
}

func TestCloseInterruptsConcurrentGracefulShutdown(t *testing.T) {
	s, _ := testServer(t, testSettings())
	entered, exited := make(chan struct{}), make(chan struct{})
	s.Router().GET("/cancel", func(c *gin.Context) {
		defer close(exited)
		close(entered)
		<-c.Request.Context().Done()
	})
	addr, err := s.Start()
	if err != nil {
		t.Fatal(err)
	}
	result := requestAsync("http://" + addr.String() + "/cancel")
	waitSignal(t, entered)
	graceful := make(chan error, 1)
	go func() { graceful <- s.Shutdown(context.Background()) }()
	select {
	case <-s.Errors():
	case <-time.After(time.Second):
		t.Fatal("shutdown did not stop listener")
	}
	var calls sync.WaitGroup
	for range 8 {
		calls.Go(func() {
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	calls.Wait()
	waitSignal(t, exited)
	if err := <-graceful; err != nil {
		t.Fatal(err)
	}
	_ = waitResult(t, result)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Shutdown(canceled); err != nil {
		t.Fatalf("completed shutdown lost idempotency: %v", err)
	}
	if err := s.Shutdown(nil); err == nil {
		t.Fatal("nil shutdown context accepted")
	}
}

func TestShutdownCanWaitAgainAfterNonCooperativeHandler(t *testing.T) {
	settings := testSettings()
	settings.ShutdownTimeout = 30 * time.Millisecond
	s, _ := testServer(t, settings)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	s.Router().GET("/blocked", func(*gin.Context) { close(entered); <-release })
	addr, err := s.Start()
	if err != nil {
		t.Fatal(err)
	}
	result := requestAsync("http://" + addr.String() + "/blocked")
	waitSignal(t, entered)
	start := time.Now()
	if err := s.Shutdown(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("shutdown added an unconfigured cleanup grace period")
	}
	unblock()
	waitSignal(t, s.closed)
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("repeated shutdown did not observe completed cleanup: %v", err)
	}
	_ = waitResult(t, result)
}
