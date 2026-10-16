/**
 * @Author: LiuShuXin
 * @Description: 验证响应体排空、强制取消以及并发请求与关闭的准入边界
 * @File: lifecycle_test.go
 * Software: Goland
 * @Date: 2026/10/14
 */
package httpclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
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

func awaitError(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("operation did not finish")
		return nil
	}
}

func TestShutdownWaitsForResponseConsumption(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "first-")
		w.(http.Flusher).Flush()
		select {
		case <-release:
			io.WriteString(w, "last")
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(server.Close)
	o := DefaultOptions()
	o.ShutdownTimeout = 2 * time.Second
	c := testClient(t, o)
	t.Cleanup(unblock)
	response, err := c.Do(testRequest(t, context.Background(), "GET", server.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	consumed := make(chan error, 1)
	go func() {
		data, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err == nil && string(data) != "first-last" {
			err = errors.New("response body incomplete")
		}
		consumed <- err
	}()
	shutdowns := make(chan error, 8)
	for range 8 {
		go func() { shutdowns <- c.Shutdown(context.Background()) }()
	}
	waitForDrain(t, c)
	if _, err := c.Do(testRequest(t, context.Background(), "GET", server.URL, nil)); !errors.Is(err, ErrClosed) {
		t.Fatalf("draining admitted another request: %v", err)
	}
	select {
	case err := <-shutdowns:
		t.Fatalf("response headers alone counted as completion: %v", err)
	default:
	}
	unblock()
	if err := awaitError(t, consumed); err != nil {
		t.Fatalf("graceful shutdown interrupted response body: %v", err)
	}
	for range 8 {
		if err := awaitError(t, shutdowns); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Shutdown(ctx); err != nil {
		t.Fatalf("completed shutdown not idempotent: %v", err)
	}
}

func TestShutdownWaitsForResponseHeaders(t *testing.T) {
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		select {
		case <-release:
			io.WriteString(w, "done")
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(server.Close)
	c := testClient(t, DefaultOptions())
	t.Cleanup(unblock)
	req := testRequest(t, context.Background(), "GET", server.URL, nil)
	consumed := make(chan error, 1)
	go func() {
		response, err := c.Do(req)
		if err == nil {
			_, err = io.Copy(io.Discard, response.Body)
			response.Body.Close()
		}
		consumed <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request did not reach server")
	}
	shutdown := make(chan error, 1)
	go func() { shutdown <- c.Shutdown(context.Background()) }()
	waitForDrain(t, c)
	unblock()
	if err := awaitError(t, consumed); err != nil {
		t.Fatalf("shutdown canceled header wait: %v", err)
	}
	if err := awaitError(t, shutdown); err != nil {
		t.Fatal(err)
	}
}

func TestShutdownForceCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/headers" {
			io.WriteString(w, "a")
			w.(http.Flusher).Flush()
		}
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	for _, name := range []string{"configured limit", "caller deadline", "caller cancellation", "Close", "unread body", "pending headers"} {
		t.Run(name, func(t *testing.T) {
			o := DefaultOptions()
			o.ShutdownTimeout = time.Second
			if name == "configured limit" || name == "unread body" || name == "pending headers" {
				o.ShutdownTimeout = 60 * time.Millisecond
			}
			c := testClient(t, o)
			consumed := make(chan error, 1)
			var response *http.Response
			if name == "pending headers" {
				req := testRequest(t, context.Background(), "GET", server.URL+"/headers", nil)
				go func() {
					resp, err := c.Do(req)
					if resp != nil {
						resp.Body.Close()
					}
					consumed <- err
				}()
				deadline := time.Now().Add(time.Second)
				for {
					c.mu.Lock()
					active := len(c.active)
					c.mu.Unlock()
					if active > 0 {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("request not admitted")
					}
					time.Sleep(time.Millisecond)
				}
			} else {
				var err error
				response, err = c.Do(testRequest(t, context.Background(), "GET", server.URL, nil))
				if err != nil {
					t.Fatal(err)
				}
				if name != "unread body" {
					go func() { _, err := io.Copy(io.Discard, response.Body); consumed <- err }()
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			if name == "caller deadline" {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 60*time.Millisecond)
			}
			defer cancel()
			shutdown := make(chan error, 1)
			start := time.Now()
			go func() { shutdown <- c.Shutdown(ctx) }()
			waitForDrain(t, c)
			if name == "caller cancellation" {
				cancel()
			}
			if name == "Close" {
				if err := c.Close(); err != nil {
					t.Fatal(err)
				}
			}
			err := awaitError(t, shutdown)
			switch name {
			case "Close":
				if err != nil {
					t.Fatal(err)
				}
			case "caller cancellation":
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("shutdown cancellation lost: %v", err)
				}
			default:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("shutdown deadline lost: %v", err)
				}
			}
			if name == "unread body" {
				_, err := io.Copy(io.Discard, response.Body)
				consumed <- err
			}
			if err := awaitError(t, consumed); !errors.Is(err, context.Canceled) {
				t.Fatalf("force close left read/Do running: %v", err)
			}
			if time.Since(start) > 700*time.Millisecond {
				t.Fatal("force close waited for original request deadline")
			}
			if response != nil {
				response.Body.Close()
			}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestConcurrentRequestsAndShutdown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	t.Cleanup(server.Close)
	c := testClient(t, DefaultOptions())
	start := make(chan struct{})
	results := make(chan error, 32)
	for range 32 {
		req := testRequest(t, context.Background(), "GET", server.URL, nil)
		go func() {
			<-start
			response, err := c.Do(req)
			if err == nil {
				_, err = io.Copy(io.Discard, response.Body)
				response.Body.Close()
			}
			results <- err
		}()
	}
	close(start)
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	for range 32 {
		if err := awaitError(t, results); err != nil && !errors.Is(err, ErrClosed) {
			t.Fatalf("admission/shutdown race interrupted successful request: %v", err)
		}
	}
}
