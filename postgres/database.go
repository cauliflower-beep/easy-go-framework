/**
 * @Author: LiuShuXin
 * @Description: 管理唯一 PostgreSQL 连接池、GORM 会话与有界资源回收
 * @File: database.go
 * Software: Goland
 * @Date: 2026/10/12
 */
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
	"go.uber.org/zap"
	driver "gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlog "gorm.io/gorm/logger"

	"github.com/cauliflower-beep/easy-go-framework/logging"
)

// Database 拥有一个 sql.DB；GORM、健康探针与事务复用该连接池。
// 会话及事务对象仅在回调内使用，不跨请求缓存。
type Database struct {
	pool        *sql.DB
	orm         *gorm.DB
	options     Options
	logger      *zap.Logger
	life        context.Context
	cancel      context.CancelFunc
	operationMu sync.Mutex
	operations  sync.WaitGroup
	closing     bool
	drainOnce   sync.Once
	drained     chan struct{}
	closeOnce   sync.Once
	closed      chan struct{}
	closeErr    error
}

// Open 先同步 Ping，失败时关闭已分配的连接池，不修改数据库结构。
func Open(ctx context.Context, options Options, logger *zap.Logger) (*Database, error) {
	if ctx == nil || logger == nil {
		return nil, errors.New("database context and logger must not be nil")
	}
	if err := options.Validate(); err != nil {
		return nil, err
	}
	config, err := connectionConfig(options)
	if err != nil {
		return nil, fmt.Errorf("configure PostgreSQL: %w", SafeError(err))
	}
	pool := stdlib.OpenDB(*config)
	pool.SetMaxOpenConns(options.MaxOpenConns)
	pool.SetMaxIdleConns(options.MaxIdleConns)
	pool.SetConnMaxLifetime(options.ConnMaxLifetime)
	pool.SetConnMaxIdleTime(options.ConnMaxIdleTime)
	pingCtx, cancel := context.WithTimeout(ctx, options.ConnectTimeout)
	err = pool.PingContext(pingCtx)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("connect PostgreSQL: %w", SafeError(errors.Join(err, pool.Close())))
	}
	orm, err := gorm.Open(driver.New(driver.Config{Conn: pool}), &gorm.Config{
		DisableAutomaticPing: true, NowFunc: func() time.Time { return time.Now().UTC() },
		Logger: queryLogger{logger: logger, slow: options.SlowThreshold, level: gormlog.Info},
	})
	if err != nil {
		return nil, fmt.Errorf("initialize database session: %w", SafeError(errors.Join(err, pool.Close())))
	}
	life, stop := context.WithCancel(context.Background())
	db := &Database{pool: pool, orm: orm, options: options, logger: logger, life: life, cancel: stop, closed: make(chan struct{})}
	logger.Info("PostgreSQL connected", zap.String("host", options.Host), zap.Int("port", options.Port), zap.String("database", options.Database), zap.Int("max_open_conns", options.MaxOpenConns))
	return db, nil
}

// scope 同时遵守调用方取消、操作期限与数据库关闭信号。
func (d *Database) scope(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc, error) {
	// 在同一把锁下登记操作与关闭入口，避免 Wait 与新操作登记并发。
	d.operationMu.Lock()
	if d.closing {
		d.operationMu.Unlock()
		return nil, nil, ErrClosed
	}
	d.operations.Add(1)
	d.operationMu.Unlock()
	work, cancel := context.WithTimeout(ctx, timeout)
	stop := context.AfterFunc(d.life, cancel)
	if d.life.Err() != nil {
		cancel()
	}
	return work, func() { stop(); cancel(); d.operations.Done() }, nil
}

// WithSession 为一次仓储操作提供有期限的独立 GORM 会话。
// 回调必须返回最终操作的 Error；禁止将会话或未关闭的 Rows 带出回调。
func (d *Database) WithSession(ctx context.Context, fn func(*gorm.DB) error) error {
	if ctx == nil || fn == nil {
		return errors.New("database context and callback must not be nil")
	}
	work, cancel, scopeErr := d.scope(ctx, d.options.QueryTimeout)
	if scopeErr != nil {
		return SafeError(scopeErr)
	}
	defer cancel()
	if err := work.Err(); err != nil {
		return SafeError(err)
	}
	err := fn(d.orm.WithContext(work))
	return SafeError(errors.Join(err, work.Err()))
}

// Transaction 由应用层组织跨仓储操作，所有参与方显式复用同一个 tx。
// 不自动重试事务，避免重复执行回调中的外部副作用。
func (d *Database) Transaction(ctx context.Context, options *sql.TxOptions, fn func(*gorm.DB) error) (err error) {
	if ctx == nil || fn == nil {
		return errors.New("transaction context and callback must not be nil")
	}
	work, cancel, scopeErr := d.scope(ctx, d.options.TransactionTimeout)
	if scopeErr != nil {
		return SafeError(scopeErr)
	}
	defer cancel()
	tx, err := d.pool.BeginTx(work, options)
	if err != nil {
		return SafeError(err)
	}
	committed := false
	defer func() {
		if !committed {
			rollbackErr := tx.Rollback()
			if rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
				d.logger.Error("database transaction rollback failed", logging.ErrorFields(SafeError(rollbackErr))...)
				err = SafeError(errors.Join(err, rollbackErr))
			}
		}
	}()
	session := d.orm.Session(&gorm.Session{NewDB: true, Context: work})
	session.Statement.ConnPool = tx
	if err = fn(session); err != nil {
		return SafeError(err)
	}
	if err = work.Err(); err != nil {
		return SafeError(err)
	}
	if err = tx.Commit(); err != nil {
		return SafeError(err)
	}
	committed = true
	return nil
}

// Ping 的期限涵盖等待池连接及服务端往返，不影响存活探针。
func (d *Database) Ping(ctx context.Context) error {
	if ctx == nil {
		return errors.New("database context must not be nil")
	}
	work, cancel, scopeErr := d.scope(ctx, d.options.HealthTimeout)
	if scopeErr != nil {
		return SafeError(scopeErr)
	}
	defer cancel()
	return SafeError(errors.Join(d.pool.PingContext(work), work.Err()))
}

// Stats 提供连接池统计，后续可直接接入指标采集。
func (d *Database) Stats() sql.DBStats { return d.pool.Stats() }

func (d *Database) beginDrain() {
	d.drainOnce.Do(func() {
		d.operationMu.Lock()
		d.closing = true
		d.drained = make(chan struct{})
		d.operationMu.Unlock()
		go func() { d.operations.Wait(); close(d.drained) }()
	})
}

func (d *Database) closeResources() {
	d.closeOnce.Do(func() {
		d.cancel()
		// 等待回调与诊断结束，再关闭池，保证后续日志回收不会抢先。
		go func() { <-d.drained; d.closeErr = d.pool.Close(); close(d.closed) }()
	})
}

func (d *Database) waitClosed(ctx context.Context) error {
	select {
	case <-d.closed:
		return SafeError(d.closeErr)
	default:
	}
	select {
	case <-d.closed:
		return SafeError(d.closeErr)
	case <-ctx.Done():
		return SafeError(ctx.Err())
	}
}

// Shutdown 拒绝新会话、事务及探针，等待已准入回调自然完成后关闭池。
// 期限取 ctx 与 ShutdownTimeout 的较短者；超时取消在途操作，后台继续回收。
// 须在回调外调用；超时后保留依赖和日志，可再次调用等待真正退出。
func (d *Database) Shutdown(ctx context.Context) error {
	if ctx == nil {
		return errors.New("database shutdown context must not be nil")
	}
	work, cancel := context.WithTimeout(ctx, d.options.ShutdownTimeout)
	defer cancel()
	d.beginDrain()
	select {
	case <-d.closed:
		return SafeError(d.closeErr)
	default:
	}
	if err := work.Err(); err != nil {
		d.closeResources()
		return SafeError(err)
	}
	select {
	case <-d.closed:
		return SafeError(d.closeErr)
	case <-d.drained:
		d.closeResources()
		return d.waitClosed(work)
	case <-work.Done():
		d.closeResources()
		return SafeError(work.Err())
	}
}

// Close 保持原有立即取消语义，再在 ShutdownTimeout 内等待回调与池回收。
// 正常退出优先使用 Shutdown；任何强制关闭都会取消全局排空。
func (d *Database) Close() error {
	work, cancel := context.WithTimeout(context.Background(), d.options.ShutdownTimeout)
	defer cancel()
	d.beginDrain()
	d.closeResources()
	return d.waitClosed(work)
}
