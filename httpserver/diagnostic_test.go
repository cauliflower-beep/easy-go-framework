/**
 * @Author: LiuShuXin
 * @Description: 验证所有拒绝路径的原因、访问日志分级和多错误记录
 * @File: diagnostic_test.go
 * Software: Goland
 * @Date: 2026/10/13
 */
package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cauliflower-beep/easy-go-framework/apperror"
	"github.com/cauliflower-beep/easy-go-framework/httpapi"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func TestRejectionDiagnosticsAcrossHTTPPipeline(t *testing.T) {
	settings := testSettings()
	settings.BodyLimit = 4096
	settings.CORS.AllowedOrigins = []string{"https://front.example"}
	s, capture := testServer(t, settings)
	s.Router().POST("/validate", httpapi.Handle(func(c *gin.Context) error {
		var input struct {
			Count int `json:"count" binding:"required,gte=1"`
		}
		if err := httpapi.BindJSON(c, &input); err != nil {
			return err
		}
		return httpapi.Success(c, input.Count)
	}))
	s.Router().GET("/permission", httpapi.Handle(func(*gin.Context) error { return apperror.New(apperror.Forbidden, 10005, "permission denied") }))
	for _, tc := range []struct {
		method, path, body, origin string
		status                     int
		code                       apperror.Code
		level, field               string
	}{
		{"POST", "/validate", `{"count":0}`, "", 400, apperror.CodeInvalidArgument, "info", "count"},
		{"POST", "/validate", `{"count":"unlabeled-private-input"}`, "", 400, apperror.CodeInvalidJSON, "info", "count"},
		{"POST", "/validate", strings.Repeat("x", 4097), "", 413, apperror.CodeBodyTooLarge, "info", ""},
		{"GET", "/permission", "", "", 403, 10005, "warn", ""},
		{"GET", "/missing", "", "", 404, apperror.CodeNotFound, "info", ""},
		{"POST", "/sample", "", "", 405, apperror.CodeMethodNotAllowed, "info", ""},
		{"GET", "/sample", "", "https://unlisted.example", 403, apperror.CodeCORSForbidden, "warn", ""},
		{"GET", "/readyz", "", "", 503, apperror.CodeNotReady, "error", ""},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		r.Header.Set("Content-Type", "application/json")
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		w := httptest.NewRecorder()
		s.Router().ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body)
		}
		var payload map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &payload)
		if _, ok := payload["data"]; ok {
			t.Fatal("error response contains data")
		}
		var entry map[string]any
		for _, candidate := range capture.entries(t) {
			if candidate["msg"] == "HTTP request completed" && candidate["request_id"] == w.Header().Get(requestIDHeader) {
				if entry != nil {
					t.Fatal("duplicate completion")
				}
				entry = candidate
			}
		}
		if entry == nil || entry["level"] != tc.level || entry["code"] != float64(tc.code) || entry["reason"] == nil {
			t.Fatalf("refusal not diagnosed: %v", entry)
		}
		if tc.field != "" {
			encoded, _ := json.Marshal(entry["details"])
			if !strings.Contains(string(encoded), tc.field) {
				t.Fatalf("field missing: %s", encoded)
			}
		}
		encoded, _ := json.Marshal(entry)
		if strings.Contains(string(encoded), "unlabeled-private-input") {
			t.Fatal("input leaked into access log")
		}
	}
}

func TestAccessLogUsesFinalStatusAndProtectsInvalidState(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		level   string
		corrupt bool
		late    bool
	}{
		{"native server failure", 500, "error", false, false},
		{"native dependency failure", 503, "error", false, false},
		{"native authentication refusal", 401, "warn", false, false},
		{"native permission refusal", 403, "warn", false, false},
		{"native bad request", 400, "info", false, false},
		{"invalid diagnostic state", 500, "error", true, false},
		{"invalid state with successful response", 200, "info", true, false},
		{"error after commit", 202, "info", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, capture := testServer(t, testSettings())
			s.Router().GET("/fixture", httpapi.Handle(func(c *gin.Context) error {
				c.String(tc.status, "fixture response")
				if tc.corrupt {
					// 模拟其他中间件覆盖框架键，不允许未知值进入日志。
					c.Set("easy-framework.httpapi.outcome", "unlabeled-private-outcome")
					c.Set("easy-framework.httpapi.errors", gin.H{"value": "unlabeled-private-summary"})
				}
				if tc.late {
					return errors.New("fixture late failure")
				}
				return nil
			}))
			w := httptest.NewRecorder()
			s.Router().ServeHTTP(w, httptest.NewRequest("GET", "/fixture", nil))
			completed, lateFailures := 0, 0
			for _, entry := range capture.entries(t) {
				if entry["msg"] == "HTTP error after response committed" {
					lateFailures++
				}
				if entry["msg"] != "HTTP request completed" {
					continue
				}
				completed++
				if entry["level"] != tc.level || entry["status"] != float64(tc.status) {
					t.Fatalf("access severity diverged from response: %v", entry)
				}
				if tc.corrupt {
					encoded, _ := json.Marshal(entry)
					if entry["diagnostic_state_invalid"] != true || entry["errors"] != nil || strings.Contains(string(encoded), "unlabeled-private") {
						t.Fatalf("invalid state escaped its boundary: %s", encoded)
					}
				}
			}
			if w.Code != tc.status || w.Body.String() != "fixture response" || completed != 1 || (tc.late && lateFailures != 1) {
				t.Fatal("response changed or request completion was lost")
			}
		})
	}
}

func TestRejectionReasonDoesNotOverwriteLogMessage(t *testing.T) {
	capture := &logCapture{}
	config := zap.NewProductionEncoderConfig()
	config.MessageKey = "message"
	logger := zap.New(zapcore.NewCore(zapcore.NewJSONEncoder(config), zapcore.AddSync(capture), zap.DebugLevel))
	s, err := New(testSettings(), logger)
	if err != nil {
		t.Fatal(err)
	}
	s.Router().GET("/fixture", httpapi.Handle(func(*gin.Context) error { return apperror.New(apperror.Conflict, 10003, "resource conflict") }))
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	w := httptest.NewRecorder()
	s.Router().ServeHTTP(w, httptest.NewRequest("GET", "/fixture", nil))
	completed := 0
	for _, entry := range capture.entries(t) {
		if entry["message"] == "HTTP request completed" {
			completed++
			if entry["reason"] != "resource conflict" || entry["code"] != float64(10003) {
				t.Fatalf("refusal metadata missing: %v", entry)
			}
		}
	}
	if completed != 1 {
		t.Fatal("rejection reason overwrote JSON log event")
	}
}

func TestPanicValueDoesNotEnterLogsOrResponse(t *testing.T) {
	s, capture := testServer(t, testSettings())
	const private = "unlabeled-private-panic-content"
	s.Router().GET("/private-panic", func(*gin.Context) { panic(private) })
	w := httptest.NewRecorder()
	s.Router().ServeHTTP(w, httptest.NewRequest("GET", "/private-panic", nil))
	if w.Code != 500 || strings.Contains(w.Body.String(), private) {
		t.Fatalf("unsafe panic response: %s", w.Body)
	}
	found := false
	for _, entry := range capture.entries(t) {
		encoded, _ := json.Marshal(entry)
		if strings.Contains(string(encoded), private) {
			t.Fatal("raw panic content entered logs")
		}
		if entry["msg"] == "HTTP panic recovered" {
			found = entry["reason"] == "handler panicked" && entry["panic_stack"] != nil && entry["request_id"] != nil
		}
	}
	if !found {
		t.Fatal("safe panic classification, stack or correlation missing")
	}
}
