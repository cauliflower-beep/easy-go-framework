/**
 * @Author: LiuShuXin
 * @Description: 使用显式提供的真实 PG 验证池、事务、取消与 SQL 日志安全
 * @File: integration_test.go
 * Software: Goland
 * @Date: 2026/10/12
 */
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cauliflower-beep/easy-go-framework/logging"
	"github.com/jackc/pgx/v5/pgconn"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"gorm.io/gorm"
)

func realOptions(t *testing.T) Options {
	t.Helper()
	host := os.Getenv("POSTGRES_TEST_HOST")
	if host == "" {
		t.Skip("real PostgreSQL integration requires POSTGRES_TEST_HOST")
	}
	o := fixtureOptions()
	o.Host = host
	o.User = os.Getenv("POSTGRES_TEST_USER")
	o.Password = os.Getenv("POSTGRES_TEST_PASSWORD")
	o.Database = os.Getenv("POSTGRES_TEST_DATABASE")
	if o.User == "" || o.Database == "" {
		t.Fatal("integration user and database must be explicit")
	}
	o.SSLMode = os.Getenv("POSTGRES_TEST_SSL_MODE")
	if o.SSLMode == "" {
		t.Fatal("integration TLS mode must be explicit")
	}
	if port := os.Getenv("POSTGRES_TEST_PORT"); port != "" {
		value, err := strconv.Atoi(port)
		if err != nil {
			t.Fatal("invalid test port")
		}
		o.Port = value
	}
	o.ConnectTimeout = 5 * time.Second
	o.QueryTimeout = 2 * time.Second
	o.TransactionTimeout = 5 * time.Second
	o.HealthTimeout = 2 * time.Second
	o.ConnMaxLifetime = 0
	o.ConnMaxIdleTime = 0
	return o
}
func realDatabase(t *testing.T, o Options, logger *zap.Logger) *Database {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d, err := Open(ctx, o, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Error(err)
		}
	})
	return d
}
func initTemporaryFixture(t *testing.T, d *Database) {
	t.Helper()
	data, err := os.ReadFile("testdata/transaction.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.WithSession(context.Background(), func(db *gorm.DB) error { return db.Exec(string(data)).Error }); err != nil {
		t.Fatal(err)
	}
}

type fixtureRow struct {
	ID    int64
	Value string
}

func (fixtureRow) TableName() string { return "framework_tx_fixture" }

func TestIntegrationPoolAndTransactions(t *testing.T) {
	o := realOptions(t)
	d := realDatabase(t, o, zap.NewNop())
	initTemporaryFixture(t, d)
	if d.orm.Statement.ConnPool != d.pool || d.Stats().MaxOpenConnections != 1 {
		t.Fatal("pool configuration not shared")
	}
	if err := d.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := d.CheckSchema(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if err := d.Transaction(context.Background(), nil, func(tx *gorm.DB) error {
		if err := tx.Create(&fixtureRow{ID: 1, Value: "committed"}).Error; err != nil {
			return err
		}
		return tx.Create(&fixtureRow{ID: 2, Value: "also-committed"}).Error
	}); err != nil {
		t.Fatal(err)
	}
	rollbackReason := errors.New("fixture rollback")
	err := d.Transaction(context.Background(), nil, func(tx *gorm.DB) error {
		if err := tx.Create(&fixtureRow{ID: 3, Value: "rolled-back"}).Error; err != nil {
			return err
		}
		return rollbackReason
	})
	if !errors.Is(err, rollbackReason) {
		t.Fatal("rollback error chain lost")
	}
	func() {
		defer func() {
			if recover() != "fixture panic" {
				t.Error("panic did not propagate")
			}
		}()
		d.Transaction(context.Background(), nil, func(tx *gorm.DB) error {
			if err := tx.Create(&fixtureRow{ID: 4, Value: "panic-rollback"}).Error; err != nil {
				t.Fatal(err)
			}
			panic("fixture panic")
		})
	}()
	var count int64
	if err := d.WithSession(context.Background(), func(db *gorm.DB) error { return db.Model(&fixtureRow{}).Count(&count).Error }); err != nil || count != 2 {
		t.Fatalf("committed rows=%d error=%v", count, err)
	}
	var readOnly string
	err = d.Transaction(context.Background(), &sql.TxOptions{ReadOnly: true}, func(tx *gorm.DB) error {
		return tx.Raw("SHOW transaction_read_only").Scan(&readOnly).Error
	})
	if err != nil || readOnly != "on" {
		t.Fatalf("read-only transaction mode=%s error=%v", readOnly, err)
	}
	var state *pgconn.PgError
	err = d.WithSession(context.Background(), func(db *gorm.DB) error { return db.Create(&fixtureRow{ID: 1, Value: "private-unique-value"}).Error })
	if !errors.As(err, &state) || state.Code != "23505" || strings.Contains(err.Error(), "private-unique-value") {
		t.Fatal("constraint state lost or value exposed")
	}
	causes, truncated := logging.Diagnostics(err)
	if truncated || len(causes) == 0 {
		t.Fatal("real constraint diagnostic missing")
	}
	diagnostic := causes[len(causes)-1]
	if diagnostic.SQLState != "23505" || diagnostic.Schema != state.SchemaName || diagnostic.Table != "framework_tx_fixture" || diagnostic.Constraint != "framework_tx_fixture_pkey" {
		t.Fatalf("real database object metadata lost: %+v", diagnostic)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var one int
			if err := d.WithSession(context.Background(), func(db *gorm.DB) error { return db.Raw("SELECT 1").Scan(&one).Error }); err != nil || one != 1 {
				t.Errorf("parallel query failed: %v", err)
			}
		}()
	}
	wg.Wait()
}

func TestIntegrationAuthenticationAndTimeouts(t *testing.T) {
	o := realOptions(t)
	bad := o
	bad.Password = "intentionally-invalid-fixture-password"
	db, err := Open(context.Background(), bad, zap.NewNop())
	var state *pgconn.PgError
	if db != nil || !errors.As(err, &state) || state.Code != "28P01" || strings.Contains(err.Error(), bad.Password) {
		if db != nil {
			db.Close()
		}
		t.Fatalf("wrong credentials not rejected safely: %v", err)
	}
	o.QueryTimeout = 150 * time.Millisecond
	d := realDatabase(t, o, zap.NewNop())
	start := time.Now()
	err = d.WithSession(context.Background(), func(db *gorm.DB) error { return db.Exec("SELECT pg_sleep(3)").Error })
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("query timeout not bounded: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := d.WithSession(ctx, func(*gorm.DB) error { t.Error("canceled callback executed"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation not propagated")
	}
}

func TestIntegrationCloseCancelsQueryAndLogsSafely(t *testing.T) {
	o := realOptions(t)
	o.QueryTimeout = 5 * time.Second
	core, logs := observer.New(zapcore.DebugLevel)
	d := realDatabase(t, o, zap.New(core))
	done := make(chan error, 1)
	go func() {
		done <- d.WithSession(context.Background(), func(db *gorm.DB) error { return db.Exec("SELECT pg_sleep(4)").Error })
	}()
	deadline := time.Now().Add(time.Second)
	for d.Stats().InUse == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if d.Stats().InUse == 0 {
		t.Fatal("query did not acquire pool connection")
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("active query survived close")
		}
	case <-time.After(time.Second):
		t.Fatal("active query not canceled")
	}
	if err := d.Close(); err != nil {
		t.Fatal("close is not idempotent")
	}
	if err := d.Ping(context.Background()); err == nil {
		t.Fatal("closed pool remains usable")
	}
	for _, entry := range logs.All() {
		for key, value := range entry.ContextMap() {
			if text, ok := value.(string); ok && (strings.Contains(text, o.Password) && o.Password != "" || strings.Contains(text, "pg_sleep")) {
				t.Fatalf("database log exposed private data in %s", key)
			}
		}
	}
}
