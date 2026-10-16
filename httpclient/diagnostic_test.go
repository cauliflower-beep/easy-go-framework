/**
 * @Author: LiuShuXin
 * @Description: 验证 HTTP 诊断、请求关联、代理及秘密数据隔离
 * @File: diagnostic_test.go
 * Software: Goland
 * @Date: 2026/10/14
 */
package httpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cauliflower-beep/easy-go-framework/logging"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestRequestDiagnosticsAndPrivacy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("X-Secret", "private-response-header")
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, "private-response-body")
	}))
	t.Cleanup(server.Close)
	core, logs := observer.New(zapcore.DebugLevel)
	c, err := New(DefaultOptions(), zap.New(core))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	ctx := logging.WithRequestID(context.Background(), "diagnostic-request")
	address := strings.Replace(server.URL, "http://", "http://private-user:private-password@", 1) + "/private-path?token=private-query#private-fragment"
	req := testRequest(t, ctx, "PRIVATE-METHOD", address, strings.NewReader("private-request-body"))
	req.Header.Set("Authorization", "Bearer private-token")
	response, err := c.Do(req)
	if err != nil || readResponse(t, response) != "private-response-body" {
		t.Fatalf("HTTP response affected by diagnostic boundary: %v", err)
	}
	entries := logs.FilterMessage("HTTP client error response").All()
	if len(entries) != 1 {
		t.Fatal("HTTP status diagnostic missing")
	}
	fields := entries[0].ContextMap()
	if fields["method"] != "OTHER" || fields["scheme"] != "http" || fields["request_id"] != "diagnostic-request" || fields["status"] != int64(502) || fields["body_complete"] != true || fields["header_duration"] == nil {
		t.Fatalf("request/status/timing correlation missing: %v", fields)
	}
	encoded, err := json.Marshal(fields)
	if err != nil || strings.Contains(string(encoded), "private") {
		t.Fatal("HTTP log exposed URL, method, header or body data")
	}
	server.Close()
	_, err = c.Do(testRequest(t, ctx, "GET", address, nil))
	if err == nil || strings.Contains(fmt.Sprintf("%+v %#v", err, err), "private") {
		t.Fatal("network error exposed request data")
	}
	var wrapped *url.Error
	if !errors.As(err, &wrapped) {
		t.Fatal("standard HTTP error chain lost")
	}
	for _, entry := range logs.All() {
		encoded, err := json.Marshal(entry.ContextMap())
		if err != nil || strings.Contains(string(encoded), "private") {
			t.Fatal("network diagnostic exposed request secrets")
		}
	}
}

func TestDiagnosticDistinguishesHTTPAndHTTPSOnSameHost(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, "unavailable")
	}))
	t.Cleanup(server.Close)
	core, logs := observer.New(zapcore.DebugLevel)
	o := DefaultOptions()
	o.SlowThreshold = 0
	c, err := New(o, zap.New(core))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	for _, endpoint := range []string{server.URL, strings.Replace(server.URL, "http://", "https://", 1)} {
		response, _ := c.Do(testRequest(t, context.Background(), "GET", endpoint+"/private?token=secret", nil))
		if response != nil {
			readResponse(t, response)
		}
	}
	entries := logs.All()
	if len(entries) != 2 {
		t.Fatalf("expected two outcome logs, got %d", len(entries))
	}
	first, second := entries[0].ContextMap(), entries[1].ContextMap()
	if first["host"] != second["host"] || first["scheme"] != "http" || second["scheme"] != "https" {
		t.Fatalf("protocol dimension missing: %v %v", first, second)
	}
}

func TestMiddlewareErrorDoesNotExposeRequestData(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	cause := errors.New("private middleware detail")
	c, err := New(DefaultOptions(), zap.New(core), func(next http.RoundTripper) http.RoundTripper {
		return RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("private request %s: %w", req.URL, cause)
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	_, err = c.Do(testRequest(t, context.Background(), "GET", "http://example.invalid/private?token=secret", nil))
	if !errors.Is(err, cause) || strings.Contains(fmt.Sprint(err), "private") {
		t.Fatal("middleware error lost cause or exposed request data")
	}
	entries := logs.FilterMessage("HTTP client request failed").All()
	if len(entries) != 1 {
		t.Fatal("middleware failure diagnostic missing")
	}
	encoded, jsonErr := json.Marshal(entries[0].ContextMap())
	if jsonErr != nil || strings.Contains(string(encoded), "private") || strings.Contains(string(encoded), "secret") {
		t.Fatal("middleware error leaked outside safe diagnostic boundary")
	}
}

func TestSlowRequestDiagnostic(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	t.Cleanup(server.Close)
	core, logs := observer.New(zapcore.DebugLevel)
	o := DefaultOptions()
	o.SlowThreshold = time.Nanosecond
	c, err := New(o, zap.New(core))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	response, err := c.Do(testRequest(t, context.Background(), "GET", server.URL, nil))
	if err != nil || readResponse(t, response) != "ok" {
		t.Fatal(err)
	}
	if logs.FilterMessage("HTTP client request slow").Len() != 1 {
		t.Fatal("slow request not logged at response completion")
	}
}

func TestHTTPProxyAuthentication(t *testing.T) {
	var observed atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "target.invalid" || r.Header.Get("Proxy-Authorization") != "Basic cHJpdmF0ZS11c2VyOnByaXZhdGUtcGFzc3dvcmQ=" {
			w.WriteHeader(407)
			return
		}
		observed.Store(true)
		io.WriteString(w, "proxied")
	}))
	t.Cleanup(proxy.Close)
	o := DefaultOptions()
	o.ProxyURL = strings.Replace(proxy.URL, "http://", "http://private-user:private-password@", 1)
	c := testClient(t, o)
	response, err := c.Do(testRequest(t, context.Background(), "GET", "http://target.invalid/resource", nil))
	if err != nil || readResponse(t, response) != "proxied" || !observed.Load() {
		t.Fatalf("configured authenticated proxy not used: %v", err)
	}
}

func TestSafeErrorClassification(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, ErrClosed, ErrBodyClosed, ErrResponseTooLarge, ErrRedirectLimit, ErrRedirectOrigin, ErrRedirectDowngrade, ErrProtocolUpgrade} {
		wrapped := &url.Error{Op: "GET", URL: "http://private:secret@example.invalid/private?token=secret", Err: cause}
		err := SafeError(wrapped)
		if !errors.Is(err, cause) || strings.Contains(fmt.Sprintf("%+v %#v", err, err), "private") || strings.Contains(err.Error(), "secret") {
			t.Fatal("error classification lost or leaked data")
		}
		if SafeError(err) != err {
			t.Fatal("SafeError not idempotent")
		}
		fields, _ := logging.Diagnostics(err)
		encoded, jsonErr := json.Marshal(fields)
		if jsonErr != nil || strings.Contains(string(encoded), "private") || strings.Contains(string(encoded), "secret") {
			t.Fatal("diagnostic boundary leaked original HTTP error")
		}
	}
	if SafeError(nil) != nil {
		t.Fatal("nil error changed")
	}
}
