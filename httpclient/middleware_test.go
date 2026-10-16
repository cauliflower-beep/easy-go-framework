/**
 * @Author: LiuShuXin
 * @Description: 验证标准 HTTP 装饰链、上下文注入、响应生命周期及构造边界
 * @File: middleware_test.go
 * Software: Goland
 * @Date: 2026/10/15
 */
package httpclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cauliflower-beep/easy-go-framework/logging"
	"go.uber.org/zap"
)

type observedBody struct {
	io.ReadCloser
	closed *atomic.Int32
}

func (b *observedBody) Close() error { b.closed.Add(1); return b.ReadCloser.Close() }

func TestMiddlewareOrderRedirectsAndRequestIsolation(t *testing.T) {
	var eventsMu sync.Mutex
	var events []string
	appendEvent := func(value string) {
		eventsMu.Lock()
		defer eventsMu.Unlock()
		events = append(events, value)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Trace") != "injected" || r.Header.Get("X-Second") != "present" {
			w.WriteHeader(400)
			return
		}
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/final", http.StatusFound)
			return
		}
		io.WriteString(w, "done")
	}))
	t.Cleanup(server.Close)
	var constructed atomic.Int32
	var closed atomic.Int32
	first := func(next http.RoundTripper) http.RoundTripper {
		constructed.Add(1)
		return RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
			appendEvent("A before " + req.URL.Path)
			clone := req.Clone(req.Context())
			clone.Header.Set("X-Trace", "injected")
			response, err := next.RoundTrip(clone)
			appendEvent("A after " + req.URL.Path)
			return response, err
		})
	}
	second := func(next http.RoundTripper) http.RoundTripper {
		constructed.Add(1)
		return RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
			appendEvent("B before " + req.URL.Path)
			if req.Header.Get("X-Trace") != "injected" || logging.RequestID(req.Context()) != "middleware-request" {
				return nil, errors.New("middleware lost injected header or request context")
			}
			clone := req.Clone(req.Context())
			clone.Header.Set("X-Second", "present")
			response, err := next.RoundTrip(clone)
			if response != nil && err == nil {
				response.Body = &observedBody{ReadCloser: response.Body, closed: &closed}
			}
			appendEvent("B after " + req.URL.Path)
			return response, err
		})
	}
	o := DefaultOptions()
	o.MaxRedirects = 1
	c, err := New(o, zap.NewNop(), first, second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	ctx := logging.WithRequestID(context.Background(), "middleware-request")
	req := testRequest(t, ctx, "GET", server.URL+"/start", nil)
	req.Header.Set("X-Trace", "caller")
	response, err := c.Do(req)
	if err != nil || readResponse(t, response) != "done" {
		t.Fatalf("decorated request failed: %v", err)
	}
	expected := []string{
		"A before /start", "B before /start", "B after /start", "A after /start",
		"A before /final", "B before /final", "B after /final", "A after /final",
	}
	if !reflect.DeepEqual(events, expected) || constructed.Load() != 2 {
		t.Fatalf("unexpected middleware order/initialization: %v builds=%d", events, constructed.Load())
	}
	if req.Header.Get("X-Trace") != "caller" || req.Header.Get("X-Second") != "" || req.Context().Err() != nil {
		t.Fatal("middleware mutated caller request")
	}
	if err := c.Shutdown(context.Background()); err != nil || closed.Load() != 2 {
		t.Fatalf("redirect/final response body ownership broken: %v closes=%d", err, closed.Load())
	}
}

func TestMiddlewareValidation(t *testing.T) {
	var typedNil *observedTransport
	for _, test := range []struct {
		name       string
		middleware Middleware
	}{
		{"nil middleware", nil},
		{"nil interface", func(http.RoundTripper) http.RoundTripper { return nil }},
		{"nil function", func(http.RoundTripper) http.RoundTripper { return RoundTripperFunc(nil) }},
		{"nil pointer", func(http.RoundTripper) http.RoundTripper { return typedNil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			if c, err := New(DefaultOptions(), zap.NewNop(), test.middleware); err == nil || c != nil {
				t.Fatal("invalid middleware accepted")
			}
		})
	}
}

type observedTransport struct{ next http.RoundTripper }

func (t *observedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.next.RoundTrip(req)
}

func TestMiddlewareKeepsStreamingDrainAndCancellation(t *testing.T) {
	for _, force := range []bool{false, true} {
		name := "graceful"
		if force {
			name = "force"
		}
		t.Run(name, func(t *testing.T) {
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
			var closed atomic.Int32
			middleware := func(next http.RoundTripper) http.RoundTripper {
				return RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
					response, err := next.RoundTrip(req)
					if response != nil && err == nil {
						response.Body = &observedBody{ReadCloser: response.Body, closed: &closed}
					}
					return response, err
				})
			}
			o := DefaultOptions()
			o.ShutdownTimeout = time.Second
			c, err := New(o, zap.NewNop(), middleware)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { c.Close() })
			t.Cleanup(unblock)
			response, err := c.Do(testRequest(t, context.Background(), "GET", server.URL, nil))
			if err != nil {
				t.Fatal(err)
			}
			consumed := make(chan error, 1)
			go func() {
				data, err := io.ReadAll(response.Body)
				response.Body.Close()
				if !force && err == nil && string(data) != "first-last" {
					err = errors.New("decorated response incomplete")
				}
				consumed <- err
			}()
			shutdown := make(chan error, 1)
			go func() { shutdown <- c.Shutdown(context.Background()) }()
			waitForDrain(t, c)
			select {
			case err := <-shutdown:
				t.Fatalf("middleware headers completed response lifecycle: %v", err)
			default:
			}
			if force {
				if err := c.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				unblock()
			}
			err = awaitError(t, consumed)
			if (force && !errors.Is(err, context.Canceled)) || (!force && err != nil) {
				t.Fatalf("middleware changed response/cancellation outcome: %v", err)
			}
			if err := awaitError(t, shutdown); err != nil || closed.Load() != 1 {
				t.Fatalf("middleware response not closed exactly once: %v closes=%d", err, closed.Load())
			}
		})
	}
}

func TestMiddlewareConcurrentRequests(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	t.Cleanup(server.Close)
	var count atomic.Int32
	c, err := New(DefaultOptions(), zap.NewNop(), func(next http.RoundTripper) http.RoundTripper {
		return RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
			count.Add(1)
			return next.RoundTrip(req)
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	results := make(chan error, 24)
	for range 24 {
		req := testRequest(t, context.Background(), "GET", server.URL, nil)
		go func() {
			response, err := c.Do(req)
			if err == nil {
				_, err = io.Copy(io.Discard, response.Body)
				response.Body.Close()
			}
			results <- err
		}()
	}
	for range 24 {
		if err := awaitError(t, results); err != nil {
			t.Fatal(err)
		}
	}
	if count.Load() != 24 {
		t.Fatal("middleware request accounting lost under concurrency")
	}
}
