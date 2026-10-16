/**
 * @Author: LiuShuXin
 * @Description: 用实际 HTTP 服务验证连接复用、响应语义与请求体所有权
 * @File: client_test.go
 * Software: Goland
 * @Date: 2026/10/14
 */
package httpclient

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

var _ interface {
	Do(*http.Request) (*http.Response, error)
} = (*Client)(nil)

func testClient(t *testing.T, options Options) *Client {
	t.Helper()
	c, err := New(options, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	return c
}

func testRequest(t *testing.T, ctx context.Context, method, address string, body io.Reader) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, address, body)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func readResponse(t *testing.T, response *http.Response) string {
	t.Helper()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestResponseSemanticsAndConnectionReuse(t *testing.T) {
	var connections atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/empty" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, "upstream unavailable")
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	t.Cleanup(server.Close)
	c := testClient(t, DefaultOptions())
	for range 2 {
		req := testRequest(t, context.Background(), http.MethodGet, server.URL+"/status", nil)
		response, err := c.Do(req)
		if err != nil || response.StatusCode != 503 {
			t.Fatalf("HTTP error status changed into transport failure: %v", err)
		}
		if got := readResponse(t, response); got != "upstream unavailable" {
			t.Fatal(got)
		}
		if req.Context().Err() != nil || req.Header.Get("Referer") != "" {
			t.Fatal("original request mutated")
		}
	}
	if connections.Load() != 1 {
		t.Fatalf("connection not reused: %d", connections.Load())
	}
	c.CloseIdleConnections()
	response, err := c.Do(testRequest(t, context.Background(), http.MethodGet, server.URL+"/empty", nil))
	if err != nil || response.StatusCode != 204 || readResponse(t, response) != "" {
		t.Fatalf("empty response failed: %v", err)
	}
	if connections.Load() != 2 {
		t.Fatal("CloseIdleConnections did not retire idle connection")
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatalf("fully consumed responses did not drain: %v", err)
	}
}

type trackedRequestBody struct {
	io.Reader
	closed atomic.Bool
}

func (b *trackedRequestBody) Close() error { b.closed.Store(true); return nil }

func TestRequestValidationAndBodyOwnership(t *testing.T) {
	c := testClient(t, DefaultOptions())
	if _, err := c.Do(nil); err == nil {
		t.Fatal("nil request accepted")
	}
	if err := c.Shutdown(nil); err == nil {
		t.Fatal("nil shutdown context accepted")
	}
	for _, name := range []string{"invalid URL", "negative timeout", "invalid body limit", "canceled", "closed"} {
		t.Run(name, func(t *testing.T) {
			body := &trackedRequestBody{Reader: strings.NewReader("private-payload")}
			req := testRequest(t, context.Background(), http.MethodPost, "http://example.invalid/private", body)
			options := RequestOptions{}
			var target error
			switch name {
			case "invalid URL":
				req.URL = nil
			case "negative timeout":
				options.Timeout = -time.Second
			case "invalid body limit":
				options.MaxResponseBytes = -2
			case "canceled":
				ctx, cancel := context.WithCancel(req.Context())
				cancel()
				req = req.WithContext(ctx)
				target = context.Canceled
			case "closed":
				if err := c.Close(); err != nil {
					t.Fatal(err)
				}
				target = ErrClosed
			}
			if _, err := c.DoWithOptions(req, options); err == nil || (target != nil && !errors.Is(err, target)) {
				t.Fatalf("unexpected request validation: %v", err)
			}
			if !body.closed.Load() {
				t.Fatal("rejected request body not closed")
			}
		})
	}
}

func TestPOSTFailureIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.Copy(io.Discard, r.Body)
		connection, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			connection.Close()
		}
	}))
	t.Cleanup(server.Close)
	c := testClient(t, DefaultOptions())
	req := testRequest(t, context.Background(), http.MethodPost, server.URL, strings.NewReader("payload"))
	if _, err := c.Do(req); err == nil || calls.Load() != 1 {
		t.Fatalf("write failure missing or repeated: %v calls=%d", err, calls.Load())
	}
}

func TestPrematureResponseEOF(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "10")
		io.WriteString(w, "tiny")
	}))
	t.Cleanup(server.Close)
	c := testClient(t, DefaultOptions())
	response, err := c.Do(testRequest(t, context.Background(), "GET", server.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	response.Body.Close()
	if string(data) != "tiny" || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated response accepted as complete: %v", err)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatalf("failed response leaked lifecycle registration: %v", err)
	}
}

func TestProtocolUpgradeRejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, writer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer connection.Close()
		writer.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: fixture\r\n\r\n")
		writer.Flush()
	}))
	t.Cleanup(server.Close)
	c := testClient(t, DefaultOptions())
	req := testRequest(t, context.Background(), "GET", server.URL, nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "fixture")
	if _, err := c.Do(req); !errors.Is(err, ErrProtocolUpgrade) {
		t.Fatalf("unsupported bidirectional response exposed as plain stream: %v", err)
	}
}

func TestGetBodyPanicDoesNotLeakRequestRegistration(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		http.Redirect(w, r, "/again", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(server.Close)
	o := DefaultOptions()
	o.MaxRedirects = 1
	c := testClient(t, o)
	req := testRequest(t, context.Background(), "POST", server.URL, strings.NewReader("payload"))
	req.GetBody = func() (io.ReadCloser, error) { panic("fixture panic") }
	func() {
		defer func() {
			if recover() != "fixture panic" {
				t.Error("caller panic not propagated")
			}
		}()
		c.Do(req)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Shutdown(ctx); err != nil {
		t.Fatalf("panic left an active request registered: %v", err)
	}
}
