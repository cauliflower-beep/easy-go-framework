/**
 * @Author: LiuShuXin
 * @Description: 验证响应字段边界、多错误诊断、敏感数据隔离和访问摘要
 * @File: diagnostic_test.go
 * Software: Goland
 * @Date: 2026/10/13
 */
package httpapi_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cauliflower-beep/easy-go-framework/apperror"
	"github.com/cauliflower-beep/easy-go-framework/httpapi"
	"github.com/cauliflower-beep/easy-go-framework/logging"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestResponseFieldsFollowSuccessOrFailure(t *testing.T) {
	for _, data := range []any{nil, false, 0, "", []string{}, map[string]any{}} {
		router, _ := testEngine()
		router.GET("/fixture", httpapi.Handle(func(c *gin.Context) error { return httpapi.Success(c, data) }))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", "/fixture", nil))
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if _, ok := payload["data"]; !ok {
			t.Fatal("success omitted data")
		}
		if _, ok := payload["details"]; ok {
			t.Fatal("success includes error details")
		}
	}
	router, _ := testEngine()
	router.GET("/fixture", httpapi.Handle(func(*gin.Context) error { return errors.New("fixture internal failure") }))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/fixture", nil))
	var payload map[string]json.RawMessage
	_ = json.Unmarshal(w.Body.Bytes(), &payload)
	if _, ok := payload["data"]; ok {
		t.Fatal("error still contains data")
	}
	if _, ok := payload["details"]; ok {
		t.Fatal("error without violations includes details")
	}
}

func TestMultipleErrorsKeepOrderAndEarlierServerFailure(t *testing.T) {
	router, logs := testEngine()
	router.GET("/fixture", func(c *gin.Context) {
		_ = c.Error(fmt.Errorf("first operation: %w", errors.New("upstream connection closed")))
		_ = c.Error(apperror.New(apperror.Conflict, 10003, "resource conflict"))
		c.Abort()
	})
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/fixture", nil))
	response := decodeResponse(t, w)
	if w.Code != 409 || response.Code != 10003 {
		t.Fatal("last-error response policy changed")
	}
	entries := logs.FilterMessage("HTTP request failed").All()
	if len(entries) != 1 {
		t.Fatal("earlier 500 masked or logged repeatedly")
	}
	fields := entries[0].ContextMap()
	encoded, _ := json.Marshal(fields["errors"])
	var events []struct {
		Index  int
		Causes []struct{ Message string }
	}
	if err := json.Unmarshal(encoded, &events); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Index != 0 || events[1].Index != 1 || !strings.Contains(string(encoded), "upstream connection closed") || !strings.Contains(string(encoded), "resource conflict") {
		t.Fatalf("incomplete ordered diagnostics: %s", encoded)
	}
}

func TestTruncationKeepsInternalFailureAndResponseError(t *testing.T) {
	router, logs := testEngine()
	router.GET("/fixture", func(c *gin.Context) {
		for index := range 30 {
			var err error = apperror.New(apperror.Conflict, 10003, "resource conflict")
			if index == 20 {
				err = errors.New("critical upstream failure")
			}
			_ = c.Error(err)
		}
		c.Abort()
	})
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/fixture", nil))
	if w.Code != 409 {
		t.Fatal("last response changed")
	}
	entries := logs.FilterMessage("HTTP request failed").All()
	if len(entries) != 1 {
		t.Fatal("internal failure hidden")
	}
	encoded, _ := json.Marshal(entries[0].ContextMap()["errors"])
	var items []struct{ Index int }
	_ = json.Unmarshal(encoded, &items)
	if len(items) != 16 || items[14].Index != 20 || items[15].Index != 29 || !strings.Contains(string(encoded), "critical upstream failure") {
		t.Fatalf("critical event truncated: %s", encoded)
	}
}

func TestErrorDiagnosticsRedactCredentialsAndValidationValues(t *testing.T) {
	router, logs := testEngine()
	router.GET("/secret", httpapi.Handle(func(*gin.Context) error { return errors.New("upstream dial failed password=fixture-password") }))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/secret", nil))
	fields := logs.All()[0].ContextMap()
	encoded, _ := json.Marshal(fields)
	if strings.Contains(string(encoded), "fixture-password") || !strings.Contains(string(encoded), "upstream dial failed") || strings.Contains(w.Body.String(), "upstream") {
		t.Fatal("diagnostic missing, secret logged or cause exposed to client")
	}
	router, logs = testEngine()
	router.POST("/input", func(c *gin.Context) {
		var input struct {
			Count int `json:"count"`
		}
		_ = c.Error(httpapi.BindJSON(c, &input))
		_ = c.Error(errors.New("secondary internal failure"))
		c.Abort()
	})
	request := httptest.NewRequest("POST", "/input", strings.NewReader(`{"count":"unlabeled-private-input"}`))
	request.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, request)
	encoded, _ = json.Marshal(logs.All()[0].ContextMap())
	if strings.Contains(string(encoded), "unlabeled-private-input") || !strings.Contains(string(encoded), "secondary internal failure") {
		t.Fatal("validation diagnostic expanded input or lost later error")
	}
}

func TestCompletionSummaryAndErrorCountBound(t *testing.T) {
	for _, tc := range []struct {
		name  string
		kind  apperror.Kind
		count int
		abort bool
		level zapcore.Level
	}{
		{"business conflict", apperror.Conflict, 1, false, zapcore.InfoLevel},
		{"authentication refusal", apperror.Unauthenticated, 1, false, zapcore.WarnLevel},
		{"internal failure", apperror.Internal, 1, false, zapcore.ErrorLevel},
		{"dependency unavailable", apperror.Unavailable, 1, false, zapcore.ErrorLevel},
		{"direct route refusal", apperror.NotFound, 0, true, zapcore.InfoLevel},
		{"bounded error collection", apperror.Conflict, 30, false, zapcore.InfoLevel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.DebugLevel)
			logger := zap.New(core)
			router := gin.New()
			// 观察器安装在 ErrorHandler 之前，回程读取已经准备好的完整结果。
			router.Use(func(c *gin.Context) {
				c.Request = c.Request.WithContext(logging.WithRequestID(c.Request.Context(), "fixture-request"))
				c.Header("X-Request-ID", "fixture-request")
			}, func(c *gin.Context) {
				c.Next()
				level, fields := httpapi.Completion(c)
				logger.Check(level, "completion").Write(fields...)
			}, httpapi.ErrorHandler(logger))
			router.GET("/fixture", func(c *gin.Context) {
				if tc.abort {
					httpapi.Abort(c, 404, apperror.CodeNotFound, "route not found")
					return
				}
				for range tc.count {
					_ = c.Error(apperror.New(tc.kind, 0, "public refusal"))
				}
				c.Abort()
			})
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest("GET", "/fixture", nil))
			entries := logs.FilterMessage("completion").All()
			if len(entries) != 1 || entries[0].Level != tc.level {
				t.Fatal("completion severity is incorrect or duplicated")
			}
			fields := entries[0].ContextMap()
			if fields["code"] == nil || fields["reason"] == nil {
				t.Fatal("rejection lacks readable reason")
			}
			if tc.count > 16 {
				encoded, _ := json.Marshal(fields["errors"])
				var items []any
				_ = json.Unmarshal(encoded, &items)
				if len(items) != 16 || fields["errors_truncated"] != true || fields["error_count"] != int64(30) {
					t.Fatalf("unbounded or incomplete collection: %v", fields)
				}
			}
		})
	}
}
