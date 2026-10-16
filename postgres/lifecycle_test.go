/**
 * @Author: LiuShuXin
 * @Description: 验证数据库关闭等待回调、并发登记与有界失败
 * @File: lifecycle_test.go
 * Software: Goland
 * @Date: 2026/10/12
 */
package postgres

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

func TestCloseWaitsForSessionCallback(t *testing.T) {
	d, mock := mockDatabase(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseOperation := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseOperation()
	operation := make(chan error, 1)
	go func() {
		operation <- d.WithSession(context.Background(), func(*gorm.DB) error { close(entered); <-release; return nil })
	}()
	<-entered
	// 清理回调会再次调用 Close，故在这里预先登记一次关闭期望。
	mock.ExpectClose()
	closing := make(chan error, 1)
	go func() { closing <- d.Close() }()
	select {
	case <-closing:
		t.Fatal("close returned before callback completed")
	case <-time.After(20 * time.Millisecond):
	}
	releaseOperation()
	if err := <-operation; !errors.Is(err, context.Canceled) {
		t.Fatalf("closing did not cancel operation: %v", err)
	}
	if err := <-closing; err != nil {
		t.Fatal(err)
	}
}

func TestCloseTimeoutIsBounded(t *testing.T) {
	pool, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	life, cancel := context.WithCancel(context.Background())
	options := fixtureOptions()
	options.ShutdownTimeout = 20 * time.Millisecond
	d := &Database{pool: pool, options: options, logger: zap.NewNop(), life: life, cancel: cancel, closed: make(chan struct{})}
	// 模拟不响应取消的回调；释放后仍能完成真正回收。
	_, release, err := d.scope(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectClose()
	start := time.Now()
	if err := d.Close(); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("close deadline not observed: %v", err)
	}
	if _, _, err := d.scope(context.Background(), time.Second); !errors.Is(err, ErrClosed) {
		t.Fatal("closing admitted another operation")
	}
	release()
	select {
	case <-d.closed:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not complete after callback released")
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func waitForDatabaseDrain(t *testing.T, d *Database) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		d.operationMu.Lock()
		closing := d.closing
		d.operationMu.Unlock()
		if closing {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("shutdown did not close admission")
}

func TestShutdownDrainsWithoutCancelingSession(t *testing.T) {
	d, mock := mockDatabase(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	operation := make(chan error, 1)
	go func() {
		operation <- d.WithSession(context.Background(), func(session *gorm.DB) error {
			close(entered)
			select {
			case <-release:
				return session.Statement.Context.Err()
			case <-session.Statement.Context.Done():
				return session.Statement.Context.Err()
			}
		})
	}()
	<-entered
	mock.ExpectClose()
	shutdowns := make(chan error, 8)
	for range 8 {
		go func() { shutdowns <- d.Shutdown(context.Background()) }()
	}
	waitForDatabaseDrain(t, d)
	if err := d.Ping(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("draining admitted probe: %v", err)
	}
	select {
	case err := <-shutdowns:
		t.Fatalf("shutdown did not wait for callback: %v", err)
	default:
	}
	unblock()
	if err := <-operation; err != nil {
		t.Fatalf("graceful shutdown canceled session: %v", err)
	}
	for range 8 {
		if err := <-shutdowns; err != nil {
			t.Fatal(err)
		}
	}
}

func TestShutdownTimeoutCancelsAndAllowsAnotherWait(t *testing.T) {
	d, mock := mockDatabase(t)
	d.options.ShutdownTimeout = 30 * time.Millisecond
	_, release, err := d.scope(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(release) }
	defer unblock()
	mock.ExpectClose()
	if err := d.Shutdown(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("configured shutdown timeout lost: %v", err)
	}
	if d.life.Err() != context.Canceled {
		t.Fatal("shutdown timeout did not cancel in-flight context")
	}
	unblock()
	select {
	case <-d.closed:
	case <-time.After(time.Second):
		t.Fatal("database did not finish cleanup")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := d.Shutdown(canceled); err != nil {
		t.Fatalf("finished shutdown lost idempotency: %v", err)
	}
	if err := d.Shutdown(nil); err == nil {
		t.Fatal("nil shutdown context accepted")
	}
}

func TestCloseInterruptsDatabaseDrain(t *testing.T) {
	d, mock := mockDatabase(t)
	work, release, err := d.scope(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(release) }
	defer unblock()
	mock.ExpectClose()
	shutdown := make(chan error, 1)
	go func() { shutdown <- d.Shutdown(context.Background()) }()
	waitForDatabaseDrain(t, d)
	closing := make(chan error, 1)
	go func() { closing <- d.Close() }()
	select {
	case <-work.Done():
	case <-time.After(time.Second):
		t.Fatal("Close did not interrupt graceful drain")
	}
	unblock()
	if err := <-closing; err != nil {
		t.Fatal(err)
	}
	if err := <-shutdown; err != nil {
		t.Fatal(err)
	}
}
