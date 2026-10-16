/**
 * @Author: LiuShuXin
 * @Description: 验证 HTTP 中间件、真实网络限制、启动失败与停机排空
 * @File: server_test.go
 * Software: Goland
 * @Date: 2026/10/10 11:10
 */

package httpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/cauliflower-beep/easy-go-framework/apperror"
	"github.com/cauliflower-beep/easy-go-framework/httpapi"
	"github.com/cauliflower-beep/easy-go-framework/logging"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	os.Exit(m.Run())
}

var requestIDPattern = regexp.MustCompile(`^req_[a-z2-7]{20}$`)

func testSettings() Options {
	return Options{
		Host: "127.0.0.1", Port: 0, ReadHeaderTimeout: time.Second, ReadTimeout: 2 * time.Second,
		WriteTimeout: 2 * time.Second, IdleTimeout: 2 * time.Second, ShutdownTimeout: time.Second,
		MaxHeaderBytes: 1024, BodyLimit: 8,
		CORS: CORS{AllowedMethods: []string{"GET", "POST", "OPTIONS"}, AllowedHeaders: []string{"Content-Type"}, MaxAge: time.Hour},
	}
}

func testServer(t *testing.T, settings Options) (*Server, *logCapture) {
	t.Helper()
	capture := &logCapture{}
	encoder := zap.NewProductionEncoderConfig()
	logger := zap.New(zapcore.NewCore(zapcore.NewJSONEncoder(encoder), zapcore.AddSync(capture), zap.DebugLevel), zap.Fields(zap.String("module", "http")))
	s, err := New(settings, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})
	s.Router().GET("/sample", func(c *gin.Context) { c.JSON(200, gin.H{"message": "sample"}) })
	return s, capture
}

func TestFoundationRoutesAndRequestCorrelation(t *testing.T) {
	s, capture := testServer(t, testSettings())
	s.Router().GET("/context", func(c *gin.Context) { c.String(200, logging.RequestID(c.Request.Context())) })
	seen := map[string]bool{}
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/sample", 200}, {"GET", "/healthz", 200}, {"GET", "/readyz", 503},
		{"GET", "/missing", 404}, {"POST", "/sample", 405}, {"GET", "/sample/", 404}, {"GET", "/context", 200},
	} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		r.Header.Set(requestIDHeader, "untrusted-client-id")
		r.Header.Set("X-Forwarded-For", "198.51.100.1")
		r.RemoteAddr = "192.0.2.1:1234"
		w := httptest.NewRecorder()
		s.Router().ServeHTTP(w, r)
		id := w.Header().Get(requestIDHeader)
		if w.Code != tc.status || !requestIDPattern.MatchString(id) || id == "untrusted-client-id" || seen[id] {
			t.Fatalf("%s %s status=%d id=%q body=%s", tc.method, tc.path, w.Code, id, w.Body)
		}
		seen[id] = true
		if tc.path == "/context" && w.Body.String() != id {
			t.Fatal("standard Context differs from response ID")
		}
		if w.Code >= 400 {
			var response map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || response["request_id"] != id {
				t.Fatalf("error correlation missing: %s %v", w.Body, err)
			}
		}
	}
	completed := 0
	for _, entry := range capture.entries(t) {
		if entry["msg"] == "HTTP request completed" {
			completed++
			if entry["request_id"] == "" || !seen[entry["request_id"].(string)] || entry["ip"] != "192.0.2.1" || entry["method"] == nil || entry["path"] == nil || entry["status"] == nil || entry["duration"] == nil {
				t.Fatalf("access fields missing: %v", entry)
			}
		}
	}
	if completed != len(seen) {
		t.Fatalf("completed logs=%d requests=%d", completed, len(seen))
	}
}

func TestConcurrentRequestIDs(t *testing.T) {
	s, _ := testServer(t, testSettings())
	var workers sync.WaitGroup
	ids := make(chan string, 48)
	for range 48 {
		workers.Go(func() {
			w := httptest.NewRecorder()
			s.Router().ServeHTTP(w, httptest.NewRequest("GET", "/sample", nil))
			ids <- w.Header().Get(requestIDHeader)
		})
	}
	workers.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		if !requestIDPattern.MatchString(id) || seen[id] {
			t.Fatalf("duplicate or empty ID: %q", id)
		}
		seen[id] = true
	}
}

func TestBodyLimitsIncludingUnreadAndChunked(t *testing.T) {
	s, _ := testServer(t, testSettings())
	s.Router().POST("/echo", func(c *gin.Context) {
		data, err := io.ReadAll(c.Request.Body)
		if err != nil {
			t.Error(err)
		}
		c.Data(200, "text/plain", data)
	})
	for _, tc := range []struct {
		name, path, body string
		unknown          bool
		status           int
	}{
		{"at limit", "/echo", "12345678", false, 200}, {"known over limit", "/echo", "123456789", false, 413},
		{"chunked over limit", "/echo", "123456789", true, 413}, {"unread body", "/sample", "123456789", true, 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			method := "POST"
			if tc.path == "/sample" {
				method = "GET"
			}
			r := httptest.NewRequest(method, tc.path, strings.NewReader(tc.body))
			if tc.unknown {
				r.ContentLength = -1
				r.TransferEncoding = []string{"chunked"}
			}
			w := httptest.NewRecorder()
			s.Router().ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status=%d body=%s", w.Code, w.Body)
			}
			if w.Code == 200 && w.Body.String() != tc.body {
				t.Fatal("middleware consumed the body")
			}
		})
	}
	t.Run("broken reader", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/echo", nil)
		r.Body = errorBody{}
		r.ContentLength = -1
		w := httptest.NewRecorder()
		s.Router().ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatalf("status=%d", w.Code)
		}
	})
}

func TestPanicRecoveryAndCommittedResponse(t *testing.T) {
	s, capture := testServer(t, testSettings())
	s.Router().GET("/panic", func(*gin.Context) { panic("sample panic") })
	s.Router().GET("/committed", func(c *gin.Context) { c.String(202, "started"); panic("late panic") })
	for _, tc := range []struct {
		path   string
		status int
	}{{"/panic", 500}, {"/sample", 200}, {"/committed", 202}, {"/sample", 200}} {
		w := httptest.NewRecorder()
		s.Router().ServeHTTP(w, httptest.NewRequest("GET", tc.path, nil))
		if w.Code != tc.status {
			t.Fatalf("%s status=%d", tc.path, w.Code)
		}
		if tc.path == "/committed" && w.Body.String() != "started" {
			t.Fatalf("committed response corrupted: %s", w.Body)
		}
	}
	panics := 0
	for _, entry := range capture.entries(t) {
		if entry["msg"] == "HTTP panic recovered" {
			panics++
			if entry["request_id"] == nil || !strings.Contains(entry["panic_stack"].(string), "server_test.go") {
				t.Fatalf("panic diagnostics missing: %v", entry)
			}
		}
	}
	if panics != 2 {
		t.Fatalf("panic events=%d", panics)
	}
}

func TestCORSAndTrustedProxy(t *testing.T) {
	settings := testSettings()
	settings.CORS.AllowedOrigins = []string{"https://front.example"}
	settings.CORS.AllowCredentials = true
	s, _ := testServer(t, settings)
	for _, method := range []string{"OPTIONS", "GET"} {
		r := httptest.NewRequest(method, "/sample", nil)
		r.Header.Set("Origin", "https://front.example")
		r.Header.Set("Access-Control-Request-Method", "GET")
		r.Header.Set("Access-Control-Request-Headers", "Content-Type")
		w := httptest.NewRecorder()
		s.Router().ServeHTTP(w, r)
		if w.Header().Get("Access-Control-Allow-Origin") != "https://front.example" || w.Header().Get("Access-Control-Allow-Credentials") != "true" || w.Header().Get(requestIDHeader) == "" {
			t.Fatalf("CORS headers missing: %v", w.Header())
		}
		if method == "OPTIONS" && w.Code != 204 {
			t.Fatalf("preflight=%d", w.Code)
		}
		if method == "GET" && !strings.Contains(strings.ToLower(w.Header().Get("Access-Control-Expose-Headers")), "x-request-id") {
			t.Fatal("request ID not exposed")
		}
	}
	r := httptest.NewRequest("GET", "/sample", nil)
	r.Header.Set("Origin", "https://unlisted.example")
	w := httptest.NewRecorder()
	s.Router().ServeHTTP(w, r)
	if w.Code != 403 || w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("unlisted origin accepted")
	}
	var forbidden httpapi.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &forbidden); err != nil || forbidden.Code != apperror.CodeCORSForbidden || forbidden.RequestID != w.Header().Get(requestIDHeader) || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("CORS rejection is not JSON: %s headers=%v", w.Body, w.Header())
	}
	settings = testSettings()
	settings.TrustedProxies = []string{"10.0.0.0/8"}
	s, capture := testServer(t, settings)
	r = httptest.NewRequest("GET", "/sample", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	r.Header.Set("X-Forwarded-For", "198.51.100.42")
	s.Router().ServeHTTP(httptest.NewRecorder(), r)
	entries := capture.entries(t)
	if entries[len(entries)-1]["ip"] != "198.51.100.42" {
		t.Fatalf("trusted proxy IP not resolved: %v", entries)
	}
}

func TestListenerStartupAndReadiness(t *testing.T) {
	s, _ := testServer(t, testSettings())
	addr, err := s.Start()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start(); err == nil {
		t.Fatal("server started twice")
	}
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get("http://" + addr.String() + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("readiness=%d", resp.StatusCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if s.ready.Load() {
		t.Fatal("stopped server remains ready")
	}
	if err := <-s.Errors(); err != nil {
		t.Fatalf("normal stop reported failure: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	settings := testSettings()
	settings.Port = listener.Addr().(*net.TCPAddr).Port
	s, _ = testServer(t, settings)
	if _, err := s.Start(); err == nil || !strings.Contains(err.Error(), "listen HTTP") {
		t.Fatalf("occupied port not returned: %v", err)
	}
}

func TestGracefulShutdownDrainsWithoutCancelingRequests(t *testing.T) {
	s, _ := testServer(t, testSettings())
	entered, release := make(chan struct{}), make(chan struct{})
	s.Router().GET("/slow", func(c *gin.Context) {
		close(entered)
		select {
		case <-release:
			c.String(200, "finished")
		case <-c.Request.Context().Done():
			t.Error("normal drain canceled in-flight request")
		}
	})
	addr, err := s.Start()
	if err != nil {
		t.Fatal(err)
	}
	result := requestAsync("http://" + addr.String() + "/slow")
	waitSignal(t, entered)
	shutdown := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		shutdown <- s.Shutdown(ctx)
	}()
	select {
	case err := <-s.Errors():
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("listener did not stop")
	}
	if s.ready.Load() {
		t.Fatal("draining server is ready")
	}
	if conn, err := net.DialTimeout("tcp", addr.String(), 100*time.Millisecond); err == nil {
		conn.Close()
		t.Fatal("listener still accepts new connections")
	}
	select {
	case err := <-shutdown:
		t.Fatalf("shutdown returned before drain: %v", err)
	default:
	}
	close(release)
	response := waitResult(t, result)
	if response.err != nil || response.status != 200 || response.body != "finished" {
		t.Fatalf("in-flight response lost: %+v", response)
	}
	select {
	case err := <-shutdown:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not finish")
	}
}

func TestShutdownTimeoutCancelsAndCanWaitForHandlerCleanup(t *testing.T) {
	s, capture := testServer(t, testSettings())
	entered, cleaned := make(chan struct{}), make(chan struct{})
	s.Router().GET("/cancel", func(c *gin.Context) {
		close(entered)
		<-c.Request.Context().Done()
		logging.ForRequest(s.logger, c.Request.Context()).Info("handler cleanup finished")
		close(cleaned)
	})
	addr, err := s.Start()
	if err != nil {
		t.Fatal(err)
	}
	result := requestAsync("http://" + addr.String() + "/cancel")
	waitSignal(t, entered)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err = s.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout not reported: %v", err)
	}
	waitSignal(t, cleaned)
	_ = waitResult(t, result)
	// 超时只保证已发起取消；再次等待成功才保证 Handler 和日志真正收尾。
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.requestMu.Lock()
	active := s.active
	s.requestMu.Unlock()
	if active != 0 {
		t.Fatal("shutdown returned before handler cleanup")
	}
	found := false
	for _, entry := range capture.entries(t) {
		if entry["msg"] == "handler cleanup finished" && entry["request_id"] != nil {
			found = true
		}
	}
	if !found {
		t.Fatal("cleanup log missing before shutdown returned")
	}
}

func TestShutdownIsBoundedForNonCooperativeHandler(t *testing.T) {
	s, _ := testServer(t, testSettings())
	entered, release, exited := make(chan struct{}), make(chan struct{}), make(chan struct{})
	s.Router().GET("/uncooperative", func(*gin.Context) {
		defer close(exited)
		close(entered)
		<-release
	})
	addr, err := s.Start()
	if err != nil {
		t.Fatal(err)
	}
	result := requestAsync("http://" + addr.String() + "/uncooperative")
	waitSignal(t, entered)
	// 测试结束释放业务协程，避免验收未完成请求时留下测试资源。
	defer func() { close(release); waitSignal(t, exited) }()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = s.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "1 HTTP handlers did not finish") {
		t.Fatalf("non-cooperative handler was not reported: %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("forced shutdown exceeded its bounded cleanup period")
	}
	_ = waitResult(t, result)
}

func TestNetworkHeaderAndBodyTimeoutLimits(t *testing.T) {
	t.Run("large header", func(t *testing.T) {
		settings := testSettings()
		settings.MaxHeaderBytes = 128
		s, _ := testServer(t, settings)
		addr, err := s.Start()
		if err != nil {
			t.Fatal(err)
		}
		conn, err := net.DialTimeout("tcp", addr.String(), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		_, err = io.WriteString(conn, "GET /sample HTTP/1.1\r\nHost: local\r\nX-Large: "+strings.Repeat("a", 10000)+"\r\n\r\n")
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 431 {
			t.Fatalf("header limit status=%d", resp.StatusCode)
		}
	})
	t.Run("slow header", func(t *testing.T) {
		settings := testSettings()
		settings.ReadHeaderTimeout = 40 * time.Millisecond
		s, _ := testServer(t, settings)
		addr, err := s.Start()
		if err != nil {
			t.Fatal(err)
		}
		conn, err := net.DialTimeout("tcp", addr.String(), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		_, _ = io.WriteString(conn, "GET /sample HTTP/1.1\r\nHost:")
		var one [1]byte
		if _, err := conn.Read(one[:]); err == nil {
			t.Fatal("slow header connection was not closed")
		}
	})
	t.Run("slow body", func(t *testing.T) {
		settings := testSettings()
		settings.ReadHeaderTimeout = 40 * time.Millisecond
		settings.ReadTimeout = 80 * time.Millisecond
		s, _ := testServer(t, settings)
		s.Router().POST("/echo", func(c *gin.Context) { c.Status(200) })
		addr, err := s.Start()
		if err != nil {
			t.Fatal(err)
		}
		conn, err := net.DialTimeout("tcp", addr.String(), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		_, _ = io.WriteString(conn, "POST /echo HTTP/1.1\r\nHost: local\r\nContent-Length: 8\r\n\r\nx")
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 408 {
			t.Fatalf("body timeout=%d", resp.StatusCode)
		}
	})
}

type errorBody struct{}

func (errorBody) Read([]byte) (int, error) { return 0, errors.New("reader failed") }
func (errorBody) Close() error             { return nil }

type logCapture struct {
	mu   sync.Mutex
	data bytes.Buffer
}

func (c *logCapture) Write(data []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.data.Write(data)
}
func (c *logCapture) entries(t *testing.T) []map[string]any {
	t.Helper()
	c.mu.Lock()
	snapshot := c.data.String()
	c.mu.Unlock()
	decoder := json.NewDecoder(strings.NewReader(snapshot))
	var entries []map[string]any
	for {
		var entry map[string]any
		err := decoder.Decode(&entry)
		if errors.Is(err, io.EOF) {
			return entries
		}
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry)
	}
}

type responseResult struct {
	status int
	body   string
	err    error
}

func requestAsync(url string) <-chan responseResult {
	result := make(chan responseResult, 1)
	go func() {
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get(url)
		if err != nil {
			result <- responseResult{err: err}
			return
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		result <- responseResult{status: resp.StatusCode, body: string(data), err: err}
	}()
	return result
}
func waitSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for handler")
	}
}
func waitResult(t *testing.T, ch <-chan responseResult) responseResult {
	t.Helper()
	select {
	case result := <-ch:
		return result
	case <-time.After(4 * time.Second):
		t.Fatal("timed out waiting for response")
		return responseResult{}
	}
}
