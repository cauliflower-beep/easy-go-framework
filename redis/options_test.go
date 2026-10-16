/**
 * @Author: LiuShuXin
 * @Description: 验证 Redis 配置边界、重试策略、TLS 与脱敏格式
 * @File: options_test.go
 * Software: Goland
 * @Date: 2026/10/14
 */
package redis

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestDriverOptionsAreIndependentAndValidated(t *testing.T) {
	o := DefaultOptions()
	a, err := o.DriverOptions()
	if err != nil {
		t.Fatal(err)
	}
	b, err := o.DriverOptions()
	if err != nil {
		t.Fatal(err)
	}
	if a == b || a.TLSConfig == b.TLSConfig {
		t.Fatal("driver settings must not be shared")
	}
	a.TLSConfig.ServerName = "changed.example"
	if b.TLSConfig.ServerName != o.Host || b.MaxRetries != -1 || !b.ContextTimeoutEnabled {
		t.Fatal("connection contract not preserved")
	}
	o.Host = ""
	if _, err = o.DriverOptions(); err == nil {
		t.Fatal("invalid connection options accepted")
	}
}

func TestOptionsValidation(t *testing.T) {
	if err := DefaultOptions().Validate(); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func(*Options)
	}{
		{"host", func(o *Options) { o.Host = "secret@localhost" }},
		{"port", func(o *Options) { o.Port = 65536 }},
		{"database", func(o *Options) { o.Database = -1 }},
		{"user", func(o *Options) { o.User = "private\nuser" }},
		{"password", func(o *Options) { o.Password = "private\x00password" }},
		{"client name", func(o *Options) { o.ClientName = "invalid name" }},
		{"CA without TLS", func(o *Options) { o.TLS = false; o.TLSRootCert = "private.pem" }},
		{"connect timeout", func(o *Options) { o.ConnectTimeout = 0 }},
		{"command timeout", func(o *Options) { o.CommandTimeout = -time.Second }},
		{"read timeout", func(o *Options) { o.ReadTimeout = 0 }},
		{"write timeout", func(o *Options) { o.WriteTimeout = 0 }},
		{"pool timeout", func(o *Options) { o.PoolTimeout = 0 }},
		{"health timeout", func(o *Options) { o.HealthTimeout = 0 }},
		{"shutdown timeout", func(o *Options) { o.ShutdownTimeout = 0 }},
		{"pool size", func(o *Options) { o.PoolSize = 0 }},
		{"idle count", func(o *Options) { o.MinIdleConns = o.PoolSize + 1 }},
		{"idle lifetime", func(o *Options) { o.ConnMaxIdleTime = 0 }},
		{"lifetime", func(o *Options) { o.ConnMaxLifetime = -1 }},
		{"slow threshold", func(o *Options) { o.SlowThreshold = -1 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			o := DefaultOptions()
			test.change(&o)
			err := o.Validate()
			if err == nil || strings.Contains(err.Error(), "private") {
				t.Fatalf("invalid or unsafe diagnostic: %v", err)
			}
		})
	}
}

func TestExplicitConnectionPolicyAndPrivacy(t *testing.T) {
	o := DefaultOptions()
	o.Host = "::1"
	o.User = "private-user"
	o.Password = "private-password"
	settings, err := clientOptions(o)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Addr != "[::1]:6379" || settings.Username != o.User || settings.Password != o.Password || settings.TLSConfig == nil || settings.TLSConfig.InsecureSkipVerify || settings.TLSConfig.MinVersion != tls.VersionTLS12 {
		t.Fatal("connection or TLS policy missing")
	}
	if settings.MaxRetries != -1 || settings.PipelinePoolSize != -1 || settings.MaxActiveConns != o.PoolSize || settings.DialerRetries != 1 || !settings.ContextTimeoutEnabled || settings.MaintNotificationsConfig.Mode != "disabled" {
		t.Fatal("implicit retries or extra pool enabled")
	}
	for _, value := range []string{fmt.Sprint(o), fmt.Sprintf("%+v", o), fmt.Sprintf("%#v", o)} {
		if strings.Contains(value, o.Password) || strings.Contains(value, o.User) {
			t.Fatal("configuration exposed identity")
		}
	}
	value, _ := json.Marshal(o)
	if strings.Contains(string(value), o.Password) {
		t.Fatal("JSON exposed password")
	}
	o.TLS = false
	o.TLSRootCert = ""
	settings, err = clientOptions(o)
	if err != nil || settings.TLSConfig != nil {
		t.Fatal("explicit plain connection not respected")
	}
	o.TLS = true
	o.TLSRootCert = t.TempDir() + "/missing-secret.pem"
	_, err = clientOptions(o)
	if err == nil || strings.Contains(err.Error(), "missing-secret") {
		t.Fatal("unsafe certificate diagnostic")
	}
}
