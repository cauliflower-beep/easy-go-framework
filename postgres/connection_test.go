/**
 * @Author: LiuShuXin
 * @Description: 验证连接配置的环境隔离、TLS 与查询日志脱敏
 * @File: connection_test.go
 * Software: Goland
 * @Date: 2026/10/12
 */
package postgres

import (
	"context"
	"crypto/tls"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cauliflower-beep/easy-go-framework/logging"
	"github.com/jackc/pgx/v5/pgconn"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	gormlog "gorm.io/gorm/logger"
)

func fixtureOptions() Options {
	return Options{Host: "127.0.0.1", Port: 5432, Database: "fixture", User: "fixture-user", Password: "fake-private-secret", SSLMode: "disable",
		ConnectTimeout: time.Second, QueryTimeout: time.Second, TransactionTimeout: 2 * time.Second, HealthTimeout: time.Second, ShutdownTimeout: time.Second,
		MaxOpenConns: 1, MaxIdleConns: 1, ConnMaxLifetime: time.Minute, ConnMaxIdleTime: time.Minute, SlowThreshold: 200 * time.Millisecond}
}

func TestConnectionConfigIsExplicit(t *testing.T) {
	t.Setenv("PGSERVICE", "")
	for k, v := range map[string]string{"PGHOST": "ignored.invalid", "PGPORT": "1234", "PGDATABASE": "ignored", "PGUSER": "ignored-user", "PGPASSWORD": "ignored-password", "PGSSLMODE": "verify-full", "PGSSLCERT": "missing.pem", "PGOPTIONS": "-c statement_timeout=0", "PGTARGETSESSIONATTRS": "standby"} {
		t.Setenv(k, v)
	}
	o := fixtureOptions()
	o.User = "reader role"
	o.Database = "db/with ' quotes"
	o.Password = " a'\\secret\n "
	c, err := connectionConfig(o)
	if err != nil {
		t.Fatal(SafeError(err))
	}
	if c.Host != o.Host || c.Port != uint16(o.Port) || c.Database != o.Database || c.User != o.User || c.Password != o.Password || c.TLSConfig != nil || len(c.Fallbacks) != 0 || c.ValidateConnect != nil {
		t.Fatal("explicit fields or single target policy lost")
	}
	if c.RuntimeParams["statement_timeout"] != "1000" || c.RuntimeParams["timezone"] != "UTC" || c.RuntimeParams["options"] != "" {
		t.Fatalf("unexpected runtime parameters: %v", c.RuntimeParams)
	}
	o.Password = ""
	c, err = connectionConfig(o)
	if err != nil || c.Password != "" {
		t.Fatal("empty password inherited ambient credential")
	}
}

func TestTLSModes(t *testing.T) {
	t.Setenv("PGSERVICE", "")
	for _, mode := range []string{"disable", "require", "verify-ca", "verify-full"} {
		t.Run(mode, func(t *testing.T) {
			o := fixtureOptions()
			o.SSLMode = mode
			c, err := connectionConfig(o)
			if err != nil {
				t.Fatal(SafeError(err))
			}
			if len(c.Fallbacks) != 0 {
				t.Fatal("implicit fallback enabled")
			}
			if mode == "disable" {
				if c.TLSConfig != nil {
					t.Fatal("plaintext unexpectedly uses TLS")
				}
				return
			}
			cfg := c.TLSConfig
			if cfg == nil || cfg.MinVersion != tls.VersionTLS12 || cfg.ServerName != o.Host {
				t.Fatal("TLS configuration incomplete")
			}
			if mode == "verify-full" && (cfg.InsecureSkipVerify || cfg.RootCAs == nil) {
				t.Fatal("host verification disabled")
			}
			if mode == "verify-ca" && (cfg.VerifyConnection == nil || cfg.RootCAs == nil) {
				t.Fatal("certificate verification disabled")
			}
		})
	}
	o := fixtureOptions()
	o.SSLMode = "verify-full"
	o.SSLRootCert = "nonexistent-ca-file.pem"
	if _, err := connectionConfig(o); err == nil {
		t.Fatal("missing CA accepted")
	}
}

func TestOpenCanceledAndInvalidInputs(t *testing.T) {
	t.Setenv("PGSERVICE", "")
	o := fixtureOptions()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	db, err := Open(ctx, o, zap.NewNop())
	if db != nil || !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), o.Password) {
		t.Fatalf("canceled open returned %v", err)
	}
	if _, err := Open(nil, o, zap.NewNop()); err == nil {
		t.Fatal("nil context accepted")
	}
	if _, err := Open(context.Background(), o, nil); err == nil {
		t.Fatal("nil logger accepted")
	}
}

func TestQueryDiagnosticsDoNotExpandSQL(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	l := queryLogger{logger: zap.New(core), slow: time.Millisecond, level: gormlog.Info}
	ctx := logging.WithRequestID(context.Background(), "fixture-request-id")
	err := &pgconn.PgError{Code: "23505", Message: "private value", Detail: "fake-private-secret"}
	l.Trace(ctx, time.Now(), func() (string, int64) { t.Fatal("SQL interpolation callback invoked"); return "", 0 }, err)
	l.Trace(ctx, time.Now().Add(-time.Second), nil, nil)
	l.Trace(ctx, time.Now(), nil, nil)
	if logs.Len() != 3 {
		t.Fatalf("diagnostics=%d", logs.Len())
	}
	for _, entry := range logs.All() {
		fields := entry.ContextMap()
		if fields["request_id"] != "fixture-request-id" || strings.Contains(entry.Message, "private") || strings.Contains(entry.Context[0].String, "fake-private-secret") {
			t.Fatal("request context missing or payload exposed")
		}
		if value, ok := fields["error"].(string); ok && strings.Contains(value, "private") {
			t.Fatal("raw driver error exposed")
		}
	}
	if !errors.As(SafeError(err), new(*pgconn.PgError)) {
		t.Fatal("SQLSTATE error chain lost")
	}
}

func TestAmbientServiceFailsExplicitly(t *testing.T) {
	t.Setenv("PGSERVICE", "unrelated-service")
	if _, err := connectionConfig(fixtureOptions()); !errors.Is(err, ErrAmbientService) {
		t.Fatalf("ambient service accepted: %v", err)
	}
}
