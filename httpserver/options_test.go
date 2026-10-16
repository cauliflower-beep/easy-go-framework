/**
 * @Author: LiuShuXin
 * @Description: 验证 HTTP 组件配置约束与应用路由隔离
 * @File: options_test.go
 * Software: Goland
 * @Date: 2026/10/10 18:00
 */

package httpserver

import (
	"context"
	"go.uber.org/zap"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOptionsValidation(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		change     func(*Options)
	}{
		{"host", "http.host", func(o *Options) { o.Host = "host:8080" }},
		{"port", "http.port", func(o *Options) { o.Port = 65536 }},
		{"header timeout", "read_header_timeout", func(o *Options) { o.ReadHeaderTimeout = 3 * time.Second }},
		{"write timeout", "write_timeout", func(o *Options) { o.WriteTimeout = 0 }},
		{"idle timeout", "idle_timeout", func(o *Options) { o.IdleTimeout = -1 }},
		{"shutdown timeout", "shutdown_timeout", func(o *Options) { o.ShutdownTimeout = 0 }},
		{"header bytes", "max_header_bytes", func(o *Options) { o.MaxHeaderBytes = 0 }},
		{"body bytes", "body_limit", func(o *Options) { o.BodyLimit = 0 }},
		{"proxy", "trusted_proxies", func(o *Options) { o.TrustedProxies = []string{"localhost"} }},
		{"origin", "allowed_origins", func(o *Options) { o.CORS.AllowedOrigins = []string{"https://front.example/path"} }},
		{"wildcard credentials", "allow_credentials", func(o *Options) { o.CORS.AllowedOrigins = []string{"*"}; o.CORS.AllowCredentials = true }},
		{"methods", "allowed_methods", func(o *Options) { o.CORS.AllowedMethods = []string{"get"} }},
		{"headers", "allowed_headers", func(o *Options) { o.CORS.AllowedHeaders = []string{"bad header"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := testSettings()
			tc.change(&settings)
			if err := settings.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate=%v want=%s", err, tc.want)
			}
		})
	}
}

func TestNoImplicitApplicationRoute(t *testing.T) {
	server, err := New(testSettings(), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer server.Shutdown(context.Background())
	recorder := httptest.NewRecorder()
	server.Router().ServeHTTP(recorder, httptest.NewRequest("GET", "/hello", nil))
	if recorder.Code != 404 {
		t.Fatalf("framework registered application route: %d", recorder.Code)
	}
}
