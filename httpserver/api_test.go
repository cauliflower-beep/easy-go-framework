/**
 * @Author: LiuShuXin
 * @Description: 验证统一错误处理接入真实 HTTP 管道并保留探针响应
 * @File: api_test.go
 * Software: Goland
 * @Date: 2026/10/13
 */
package httpserver

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cauliflower-beep/easy-go-framework/apperror"
	"github.com/cauliflower-beep/easy-go-framework/httpapi"
)

func TestUnifiedAPIOverNetwork(t *testing.T) {
	s, capture := testServer(t, testSettings())
	s.Router().GET("/success", httpapi.Handle(func(c *gin.Context) error { return httpapi.Success(c, map[string]int{"count": 1}) }))
	s.Router().GET("/error", httpapi.Handle(func(*gin.Context) error { return errors.New("private-error-value") }))
	s.Router().GET("/internal-code", httpapi.Handle(func(*gin.Context) error {
		return apperror.Wrap(apperror.Internal, apperror.Code(15001), "private-internal-message", errors.New("upstream connection closed"))
	}))
	s.Router().GET("/typed-nil", httpapi.Handle(func(*gin.Context) error {
		var missing *apperror.Error
		return missing
	}))
	s.Router().GET("/ok", httpapi.Handle(httpapi.OK))
	s.Router().GET("/request-id", httpapi.Handle(func(c *gin.Context) error {
		return httpapi.Success(c, httpapi.RequestID(c))
	}))
	s.Router().GET("/conflict", httpapi.Handle(func(*gin.Context) error {
		return apperror.New(apperror.Conflict, apperror.Code(10003), "resource conflict")
	}))
	s.Router().GET("/serialization", httpapi.Handle(func(c *gin.Context) error { return httpapi.Success(c, make(chan int)) }))
	s.Router().GET("/panic-api", httpapi.Handle(func(*gin.Context) error { panic("fixture panic") }))
	s.Router().GET("/raw-error", func(c *gin.Context) { c.JSON(500, gin.H{"status": "failed"}) })
	s.Router().GET("/raw-unavailable", func(c *gin.Context) { c.JSON(503, gin.H{"status": "unavailable"}) })
	address, err := s.Start()
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	for _, tc := range []struct {
		path   string
		status int
		code   apperror.Code
	}{
		{"/success", 200, apperror.CodeOK}, {"/error", 500, apperror.CodeInternalError}, {"/serialization", 500, apperror.CodeInternalError}, {"/conflict", 409, apperror.Code(10003)},
		{"/panic-api", 500, apperror.CodeInternalError}, {"/missing", 404, apperror.CodeNotFound}, {"/healthz", 200, -1}, {"/readyz", 200, -1},
		{"/internal-code", 500, apperror.Code(15001)}, {"/typed-nil", 500, apperror.CodeInternalError}, {"/ok", 200, apperror.CodeOK}, {"/request-id", 200, apperror.CodeOK},
		{"/raw-error", 500, -1}, {"/raw-unavailable", 503, -1},
	} {
		response, err := client.Get("http://" + address.String() + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("%s: %s", tc.path, body)
		}
		if response.StatusCode != tc.status || response.Header.Get(requestIDHeader) == "" {
			t.Fatalf("%s: %d %s", tc.path, response.StatusCode, body)
		}
		if tc.code >= 0 && (payload["code"] != float64(tc.code) || payload["request_id"] != response.Header.Get(requestIDHeader)) {
			t.Fatalf("%s contract=%v", tc.path, payload)
		}
		if tc.code < 0 && (payload["status"] == nil || payload["code"] != nil) {
			t.Fatal("probe was wrapped into business envelope")
		}
		if tc.path == "/request-id" && payload["data"] != response.Header.Get(requestIDHeader) {
			t.Fatal("helper generated a different request ID")
		}
		if tc.code >= 0 && tc.status == 500 && payload["message"] != "internal server error" {
			t.Fatal("internal error message was exposed")
		}
	}
	failed, panics, internalCompletion := 0, 0, 0
	for _, entry := range capture.entries(t) {
		if entry["msg"] == "HTTP request completed" && entry["status"].(float64) >= 500 && entry["level"] != "error" {
			t.Fatalf("5xx access log was downgraded: %v", entry)
		}
		if entry["msg"] == "HTTP request failed" {
			failed++
		}
		if entry["msg"] == "HTTP panic recovered" {
			panics++
		}
		if entry["msg"] == "HTTP request completed" && entry["path"] == "/internal-code" {
			if entry["code"] != float64(15001) || entry["reason"] != "internal server error" {
				t.Fatal("access log lost the public internal code or exposed its message")
			}
			internalCompletion++
		}
	}
	if failed != 4 || panics != 1 || internalCompletion != 1 {
		t.Fatalf("duplicate or missing diagnostics: failures=%d panics=%d completions=%d", failed, panics, internalCompletion)
	}
}
