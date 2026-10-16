/**
 * @Author: LiuShuXin
 * @Description: 验证出站 HTTP 配置边界、代理凭证保护与默认期限
 * @File: options_test.go
 * Software: Goland
 * @Date: 2026/10/14
 */
package httpclient

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestOptionsValidationAndPrivacy(t *testing.T) {
	if err := DefaultOptions().Validate(); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func(*Options)
	}{
		{"request timeout", func(o *Options) { o.RequestTimeout = 0 }},
		{"dial timeout", func(o *Options) { o.DialTimeout = -time.Second }},
		{"TLS timeout", func(o *Options) { o.TLSHandshakeTimeout = 0 }},
		{"header timeout", func(o *Options) { o.ResponseHeaderTimeout = 0 }},
		{"continue timeout", func(o *Options) { o.ExpectContinueTimeout = 0 }},
		{"idle timeout", func(o *Options) { o.IdleConnTimeout = 0 }},
		{"shutdown timeout", func(o *Options) { o.ShutdownTimeout = 0 }},
		{"slow threshold", func(o *Options) { o.SlowThreshold = -time.Second }},
		{"idle limit", func(o *Options) { o.MaxIdleConns = 0 }},
		{"per-host idle limit", func(o *Options) { o.MaxIdleConnsPerHost = 0 }},
		{"per-host active limit", func(o *Options) { o.MaxConnsPerHost = 0 }},
		{"idle overflow", func(o *Options) { o.MaxIdleConnsPerHost = o.MaxIdleConns + 1 }},
		{"per-host overflow", func(o *Options) { o.MaxConnsPerHost = 1 }},
		{"header limit", func(o *Options) { o.MaxResponseHeaderBytes = 0 }},
		{"body limit", func(o *Options) { o.MaxResponseBytes = 0 }},
		{"invalid unlimited", func(o *Options) { o.MaxResponseBytes = -2 }},
		{"redirect limit", func(o *Options) { o.MaxRedirects = -1 }},
		{"CA path", func(o *Options) { o.TLSRootCert = "private\nfile" }},
		{"proxy scheme", func(o *Options) { o.ProxyURL = "socks5://private:secret@example.invalid" }},
		{"proxy path", func(o *Options) { o.ProxyURL = "http://private:secret@example.invalid/path" }},
		{"proxy query", func(o *Options) { o.ProxyURL = "http://private:secret@example.invalid?token=secret" }},
		{"proxy conflict", func(o *Options) { o.ProxyURL = "http://private:secret@example.invalid"; o.UseEnvironmentProxy = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			o := DefaultOptions()
			test.change(&o)
			err := o.Validate()
			if err == nil || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "secret") {
				t.Fatalf("invalid configuration accepted or exposed: %v", err)
			}
		})
	}
	o := DefaultOptions()
	o.ProxyURL = "http://private:secret@example.invalid:8080"
	if err := o.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{fmt.Sprint(o), fmt.Sprintf("%+v %#v", o, o)} {
		if strings.Contains(value, "private") || strings.Contains(value, "secret") {
			t.Fatal("proxy credentials exposed by formatting")
		}
	}
	encoded, err := json.Marshal(o)
	if err != nil || strings.Contains(string(encoded), "secret") {
		t.Fatal("proxy credentials exposed in JSON")
	}
	if c, err := New(DefaultOptions(), nil); err == nil || c != nil {
		t.Fatal("nil logger accepted")
	}
	o = DefaultOptions()
	o.TLSRootCert = t.TempDir() + "/private-missing.pem"
	if c, err := New(o, zap.NewNop()); err == nil || c != nil || strings.Contains(err.Error(), "private") {
		t.Fatal("missing CA diagnostic unsafe")
	}
}
