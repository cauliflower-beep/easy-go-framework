/**
 * @Author: LiuShuXin
 * @Description: 验证 JSON 契约、错误映射、日志安全与已提交响应保护
 * @File: response_test.go
 * Software: Goland
 * @Date: 2026/10/13
 */
package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/cauliflower-beep/easy-go-framework/apperror"
	"github.com/cauliflower-beep/easy-go-framework/httpapi"
	"github.com/cauliflower-beep/easy-go-framework/logging"
)

func testEngine() (*gin.Engine, *observer.ObservedLogs) {
	gin.SetMode(gin.TestMode)
	core, logs := observer.New(zapcore.DebugLevel)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(logging.WithRequestID(c.Request.Context(), "fixture-request"))
		c.Header("X-Request-ID", "fixture-request")
	}, httpapi.ErrorHandler(zap.New(core)))
	return router, logs
}

type wireResponse struct {
	httpapi.ErrorResponse
	Data any `json:"data"`
}

func decodeResponse(t *testing.T, w *httptest.ResponseRecorder) wireResponse {
	t.Helper()
	var response wireResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("invalid JSON: %s: %v", w.Body, err)
	}
	if response.RequestID != "fixture-request" || response.RequestID != w.Header().Get("X-Request-ID") || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("invalid metadata: %+v headers=%v", response, w.Header())
	}
	return response
}

func TestSuccessAndSerializationFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		data   any
		want   int
		code   apperror.Code
	}{
		{"success", 200, map[string]any{"count": 2}, 200, apperror.CodeOK},
		{"created", 201, []int{1, 2}, 201, apperror.CodeOK},
		{"empty data", 200, nil, 200, apperror.CodeOK},
		{"invalid data", 200, make(chan int), 500, apperror.CodeInternalError},
		{"no content is not JSON", 204, nil, 500, apperror.CodeInternalError},
		{"invalid status", 404, nil, 500, apperror.CodeInternalError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router, logs := testEngine()
			router.GET("/fixture", httpapi.Handle(func(c *gin.Context) error { return httpapi.Respond(c, tc.status, tc.data) }))
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest("GET", "/fixture", nil))
			response := decodeResponse(t, w)
			if w.Code != tc.want || response.Code != tc.code {
				t.Fatalf("status=%d response=%+v", w.Code, response)
			}
			if tc.want == 500 && (response.Data != nil || logs.FilterMessage("HTTP request failed").Len() != 1) {
				t.Fatal("failure not normalized and logged once")
			}
		})
	}
}

func TestApplicationErrorMappingAndPrivacy(t *testing.T) {
	private := errors.New("private-credential-and-SQL-value")
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   apperror.Code
	}{
		{"invalid", apperror.New(apperror.InvalidArgument, apperror.Code(10001), "invalid parameters"), 400, apperror.Code(10001)},
		{"unauthenticated", apperror.New(apperror.Unauthenticated, 0, ""), 401, apperror.CodeUnauthenticated},
		{"forbidden", apperror.New(apperror.Forbidden, 0, ""), 403, apperror.CodeForbidden},
		{"zero failure code", apperror.New(apperror.Conflict, 0, "conflict"), 409, apperror.CodeConflict},
		{"negative failure code", apperror.New(apperror.Conflict, -1, "conflict"), 409, apperror.CodeConflict},
		{"missing", apperror.New(apperror.NotFound, apperror.Code(10002), "resource missing"), 404, apperror.Code(10002)},
		{"wrapped conflict", fmt.Errorf("operation: %w", apperror.Wrap(apperror.Conflict, apperror.Code(10003), "resource exists", private)), 409, apperror.Code(10003)},
		{"unavailable", apperror.Wrap(apperror.Unavailable, apperror.Code(10004), "temporarily unavailable", private), 503, apperror.Code(10004)},
		{"internal", apperror.Wrap(apperror.Internal, apperror.Code(19999), private.Error(), private), 500, apperror.Code(19999)},
		{"wrapped internal", fmt.Errorf("operation: %w", apperror.Wrap(apperror.Internal, apperror.Code(15001), private.Error(), private)), 500, apperror.Code(15001)},
		{"internal zero code", apperror.New(apperror.Internal, 0, private.Error()), 500, apperror.CodeInternalError},
		{"internal negative code", apperror.New(apperror.Internal, -1, private.Error()), 500, apperror.CodeInternalError},
		{"unknown category", apperror.New(apperror.Kind(100), apperror.Code(19999), private.Error()), 500, apperror.CodeInternalError},
		{"unknown cause", private, 500, apperror.CodeInternalError},
		{"deadline without domain classification", context.DeadlineExceeded, 500, apperror.CodeInternalError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router, logs := testEngine()
			router.GET("/fixture", httpapi.Handle(func(*gin.Context) error { return tc.err }))
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest("GET", "/fixture", nil))
			response := decodeResponse(t, w)
			if w.Code != tc.status || response.Code != tc.code || response.Data != nil || strings.Contains(w.Body.String(), "private") {
				t.Fatalf("unsafe mapping: %d %s", w.Code, w.Body)
			}
			wantLogs := 0
			if tc.status >= 500 {
				wantLogs = 1
			}
			if logs.Len() != wantLogs {
				t.Fatalf("error diagnostics=%d want=%d", logs.Len(), wantLogs)
			}
			for _, entry := range logs.All() {
				if entry.ContextMap()["code"] != int32(tc.code) || (response.Message != "internal server error" && tc.status == 500) {
					t.Fatal("internal response or diagnostic code diverged")
				}
				encoded, _ := json.Marshal(entry.ContextMap())
				if len(encoded) == 0 || entry.ContextMap()["request_id"] != "fixture-request" {
					t.Fatal("missing or uncorrelated diagnostics")
				}
			}
		})
	}
}

func TestErrorAfterCommitAndGinErrorForwarding(t *testing.T) {
	router, logs := testEngine()
	router.GET("/committed", httpapi.Handle(func(c *gin.Context) error { c.String(202, "started"); return errors.New("private late error") }))
	router.GET("/gin", func(c *gin.Context) {
		_ = c.Error(apperror.New(apperror.Conflict, apperror.Code(10003), "resource conflict"))
		c.Abort()
	})
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/committed", nil))
	if w.Code != 202 || w.Body.String() != "started" || logs.FilterMessage("HTTP error after response committed").Len() != 1 {
		t.Fatal("committed response overwritten or not diagnosed")
	}
	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/gin", nil))
	if w.Code != 409 || decodeResponse(t, w).Code != apperror.Code(10003) {
		t.Fatal("Gin error bypassed central mapping")
	}
}

func TestProtocolAbortAndNoContent(t *testing.T) {
	router, _ := testEngine()
	router.GET("/abort", func(c *gin.Context) { httpapi.Abort(c, 413, apperror.CodeBodyTooLarge, "request body exceeds limit") })
	router.OPTIONS("/empty", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/abort", nil))
	if w.Code != 413 || decodeResponse(t, w).Data != nil {
		t.Fatal("protocol error contract lost")
	}
	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("OPTIONS", "/empty", nil))
	if w.Code != 204 || w.Body.Len() != 0 {
		t.Fatal("no-content response wrapped")
	}
}

func TestTypedNilErrorsReturnInternalFailure(t *testing.T) {
	var missing *apperror.Error
	for _, tc := range []struct {
		name string
		err  error
		code apperror.Code
	}{
		{"direct", missing, apperror.CodeInternalError},
		{"wrapped", fmt.Errorf("operation: %w", missing), apperror.CodeInternalError},
		{"joined", errors.Join(missing, errors.New("another failure")), apperror.CodeInternalError},
		{"internal cause", apperror.Wrap(apperror.Internal, apperror.Code(15001), "private failure", missing), apperror.Code(15001)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router, logs := testEngine()
			// 不安装 Recovery，确保错误登记与映射自身不触发 panic。
			router.GET("/fixture", httpapi.Handle(func(*gin.Context) error { return tc.err }))
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest("GET", "/fixture", nil))
			response := decodeResponse(t, w)
			if w.Code != 500 || response.Code != tc.code || response.Message != "internal server error" {
				t.Fatalf("typed-nil escaped failure mapping: %d %s", w.Code, w.Body)
			}
			entries := logs.FilterMessage("HTTP request failed").All()
			if len(entries) != 1 || entries[0].Level != zapcore.ErrorLevel || entries[0].ContextMap()["code"] != int32(tc.code) {
				t.Fatal("typed-nil diagnostic was lost")
			}
		})
	}
}

func TestOKAndRequestIDContract(t *testing.T) {
	router, logs := testEngine()
	router.GET("/fixture", httpapi.Handle(func(c *gin.Context) error {
		if httpapi.RequestID(c) != "fixture-request" {
			t.Fatal("request ID differs from the context")
		}
		return httpapi.OK(c)
	}))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/fixture", nil))
	response := decodeResponse(t, w)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || response.Code != apperror.CodeOK || response.Message != "success" || string(fields["data"]) != "null" || len(fields) != 4 || logs.Len() != 0 {
		t.Fatalf("invalid empty success contract: %d %s", w.Code, w.Body)
	}
}
