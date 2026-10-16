/**
 * @Author: LiuShuXin
 * @Description: 验证 PostgreSQL 配置边界与凭证脱敏，不依赖真实数据库
 * @File: options_test.go
 * Software: Goland
 * @Date: 2026/10/12
 */
package postgres_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cauliflower-beep/easy-go-framework/postgres"
)

func validOptions() postgres.Options {
	return postgres.Options{Host: "db.internal", Port: 5432, Database: "sampledb", User: "sample-user",
		Password: "unit-test-private-password", SSLMode: "require", ConnectTimeout: 5 * time.Second, QueryTimeout: 10 * time.Second, TransactionTimeout: 15 * time.Second, HealthTimeout: 2 * time.Second, ShutdownTimeout: 10 * time.Second, SlowThreshold: 200 * time.Millisecond,
		MaxOpenConns: 10, MaxIdleConns: 5, ConnMaxLifetime: time.Hour, ConnMaxIdleTime: time.Minute}
}

func TestOptionsValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*postgres.Options)
		want   string
	}{
		{"valid", func(o *postgres.Options) {}, ""},
		{"IPv6", func(o *postgres.Options) { o.Host = "::1" }, ""},
		{"empty password", func(o *postgres.Options) { o.Password = "" }, ""},
		{"password whitespace preserved", func(o *postgres.Options) { o.Password = " ' secret \\ \n " }, ""},
		{"zero idle and time limits", func(o *postgres.Options) { o.MaxIdleConns = 0; o.ConnMaxLifetime = 0; o.ConnMaxIdleTime = 0 }, ""},
		{"explicit plaintext", func(o *postgres.Options) { o.SSLMode = "disable" }, ""},
		{"verify CA", func(o *postgres.Options) { o.SSLMode = "verify-ca"; o.SSLRootCert = "ca.pem" }, ""},
		{"verify host", func(o *postgres.Options) { o.SSLMode = "verify-full"; o.SSLRootCert = "ca.pem" }, ""},
		{"host control character", func(o *postgres.Options) { o.Host = "db\x1b.internal" }, "postgres.host"},
		{"missing host", func(o *postgres.Options) { o.Host = "" }, "postgres.host"},
		{"host contains port", func(o *postgres.Options) { o.Host = "db.internal:5432" }, "postgres.host"},
		{"multiple hosts", func(o *postgres.Options) { o.Host = "host-a,host-b" }, "postgres.host"},
		{"host contains path", func(o *postgres.Options) { o.Host = "/tmp" }, "postgres.host"},
		{"host contains whitespace", func(o *postgres.Options) { o.Host = " db.internal" }, "postgres.host"},
		{"zero port", func(o *postgres.Options) { o.Port = 0 }, "postgres.port"},
		{"negative port", func(o *postgres.Options) { o.Port = -1 }, "postgres.port"},
		{"excessive port", func(o *postgres.Options) { o.Port = 65536 }, "postgres.port"},
		{"blank database", func(o *postgres.Options) { o.Database = " " }, "postgres.database"},
		{"database contains NUL", func(o *postgres.Options) { o.Database = "db\x00name" }, "postgres.database"},
		{"user control character", func(o *postgres.Options) { o.User = "reader\x7f" }, "postgres.user"},
		{"missing user", func(o *postgres.Options) { o.User = "" }, "postgres.user"},
		{"user surrounding whitespace", func(o *postgres.Options) { o.User = "reader " }, "postgres.user"},
		{"password contains NUL", func(o *postgres.Options) { o.Password += "\x00" }, "postgres.password"},
		{"unknown TLS mode", func(o *postgres.Options) { o.SSLMode = "invalid" }, "postgres.ssl_mode"},
		{"implicit fallback", func(o *postgres.Options) { o.SSLMode = "prefer" }, "postgres.ssl_mode"},
		{"CA without TLS", func(o *postgres.Options) { o.SSLMode = "disable"; o.SSLRootCert = "ca.pem" }, "postgres.ssl_root_cert"},
		{"CA path control", func(o *postgres.Options) { o.SSLRootCert = "ca\n.pem" }, "postgres.ssl_root_cert"},
		{"zero query timeout", func(o *postgres.Options) { o.QueryTimeout = 0 }, "postgres.query_timeout"},
		{"negative transaction timeout", func(o *postgres.Options) { o.TransactionTimeout = -time.Second }, "postgres.transaction_timeout"},
		{"zero health timeout", func(o *postgres.Options) { o.HealthTimeout = 0 }, "postgres.health_timeout"},
		{"zero shutdown timeout", func(o *postgres.Options) { o.ShutdownTimeout = 0 }, "postgres.shutdown_timeout"},
		{"negative slow threshold", func(o *postgres.Options) { o.SlowThreshold = -time.Second }, "postgres.slow_threshold"},
		{"overlong application name", func(o *postgres.Options) { o.ApplicationName = strings.Repeat("x", 64) }, "postgres.application_name"},
		{"system CA requires full verification", func(o *postgres.Options) { o.SSLRootCert = "system" }, "postgres.ssl_root_cert"},
		{"server duration exceeds range", func(o *postgres.Options) { o.QueryTimeout = 30 * 24 * time.Hour }, "PostgreSQL limits"},
		{"zero connection timeout", func(o *postgres.Options) { o.ConnectTimeout = 0 }, "postgres.connect_timeout"},
		{"negative connection timeout", func(o *postgres.Options) { o.ConnectTimeout = -time.Second }, "postgres.connect_timeout"},
		{"unlimited pool", func(o *postgres.Options) { o.MaxOpenConns = 0 }, "postgres.max_open_conns"},
		{"negative idle count", func(o *postgres.Options) { o.MaxIdleConns = -1 }, "postgres.max_idle_conns"},
		{"idle exceeds capacity", func(o *postgres.Options) { o.MaxIdleConns = 11 }, "postgres.max_idle_conns"},
		{"negative lifetime", func(o *postgres.Options) { o.ConnMaxLifetime = -time.Second }, "postgres.conn_max_lifetime"},
		{"negative idle time", func(o *postgres.Options) { o.ConnMaxIdleTime = -time.Second }, "postgres.conn_max_idle_time"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options := validOptions()
			tc.change(&options)
			err := options.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v want field=%q", err, tc.want)
			}
			if err != nil && strings.Contains(err.Error(), options.Password) && options.Password != "" {
				t.Fatal("validation error exposed password")
			}
		})
	}
}

func TestValidationReportsAllProblems(t *testing.T) {
	err := (postgres.Options{}).Validate()
	if err == nil {
		t.Fatal("missing configuration accepted")
	}
	for _, key := range []string{"host", "port", "database", "user", "ssl_mode", "connect_timeout", "max_open_conns"} {
		if !strings.Contains(err.Error(), "postgres."+key) {
			t.Fatalf("missing %s in %v", key, err)
		}
	}
}

func TestPasswordRedaction(t *testing.T) {
	options := validOptions()
	for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
		for _, value := range []any{options, &options} {
			text := fmt.Sprintf(format, value)
			if strings.Contains(text, options.Password) || !strings.Contains(text, "[REDACTED]") {
				t.Fatalf("unsafe format=%s output=%s", format, text)
			}
		}
	}
	data, err := json.Marshal(options)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if _, exists := fields["Password"]; exists || strings.Contains(string(data), options.Password) {
		t.Fatal("JSON exposed password")
	}
	if options.Password != "unit-test-private-password" {
		t.Fatal("redaction modified credentials")
	}
}
