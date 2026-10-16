/**
 * @Author: LiuShuXin
 * @Description: 验证依赖就绪状态变化、恢复、超时与存活探针隔离
 * @File: readiness_test.go
 * Software: Goland
 * @Date: 2026/10/12
 */
package httpserver

import (
	"context"
	"errors"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestReadinessDependencyTransitions(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	var failed atomic.Bool
	s, err := New(testSettings(), zap.New(core), WithReadinessCheck(time.Second, func(context.Context) error {
		if failed.Load() {
			return errors.New("secret dependency diagnostic")
		}
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown(context.Background())
	request := func(path string, status int) {
		t.Helper()
		r := httptest.NewRecorder()
		s.Router().ServeHTTP(r, httptest.NewRequest("GET", path, nil))
		if r.Code != status {
			t.Fatalf("%s status=%d body=%s", path, r.Code, r.Body)
		}
	}
	request("/readyz", 200)
	failed.Store(true)
	request("/readyz", 503)
	request("/readyz", 503)
	request("/healthz", 200)
	failed.Store(false)
	request("/readyz", 200)
	if logs.FilterMessage("HTTP readiness dependency unavailable").Len() != 1 || logs.FilterMessage("HTTP readiness dependency recovered").Len() != 1 {
		t.Fatal("dependency state changes not logged once")
	}
}

func TestReadinessTimeoutAndInvalidOptions(t *testing.T) {
	settings := testSettings()
	for _, option := range []Option{nil, WithReadinessCheck(0, func(context.Context) error { return nil }), WithReadinessCheck(time.Second, nil)} {
		if s, err := New(settings, zap.NewNop(), option); err == nil {
			if s != nil {
				s.Shutdown(context.Background())
			}
			t.Fatal("invalid readiness option accepted")
		}
	}
	s, err := New(settings, zap.NewNop(), WithReadinessCheck(20*time.Millisecond, func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown(context.Background())
	r := httptest.NewRecorder()
	start := time.Now()
	s.Router().ServeHTTP(r, httptest.NewRequest("GET", "/readyz", nil))
	if r.Code != 503 || time.Since(start) > time.Second {
		t.Fatalf("probe status=%d elapsed=%s", r.Code, time.Since(start))
	}
}
