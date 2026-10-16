/**
 * @Author: LiuShuXin
 * @Description: 隔离验证数据库事务、版本检查及关闭语义
 * @File: database_test.go
 * Software: Goland
 * @Date: 2026/10/12
 */
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/jackc/pgx/v5/pgconn"
	"go.uber.org/zap"
	driver "gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlog "gorm.io/gorm/logger"
)

func mockDatabase(t *testing.T) (*Database, sqlmock.Sqlmock) {
	t.Helper()
	pool, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	orm, err := gorm.Open(driver.New(driver.Config{Conn: pool}), &gorm.Config{DisableAutomaticPing: true, Logger: queryLogger{logger: zap.NewNop(), level: gormlog.Info}})
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	life, cancel := context.WithCancel(context.Background())
	d := &Database{pool: pool, orm: orm, options: fixtureOptions(), logger: zap.NewNop(), life: life, cancel: cancel, closed: make(chan struct{})}
	if orm.Statement.ConnPool != pool {
		t.Fatal("GORM does not share the SQL pool")
	}
	t.Cleanup(func() {
		d.operationMu.Lock()
		alreadyClosing := d.closing
		d.operationMu.Unlock()
		if !alreadyClosing {
			mock.ExpectClose()
		}
		if err := d.Close(); err != nil {
			t.Error(err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})
	return d, mock
}

func TestTransactionOutcomes(t *testing.T) {
	rollbackReason := errors.New("business rollback")
	for _, mode := range []string{"commit", "rollback", "panic", "commit failure"} {
		t.Run(mode, func(t *testing.T) {
			d, mock := mockDatabase(t)
			mock.ExpectBegin()
			mock.ExpectExec("UPDATE fixture").WithArgs("value", int64(1)).WillReturnResult(sqlmock.NewResult(0, 1))
			if mode == "commit" {
				mock.ExpectCommit()
			} else if mode == "commit failure" {
				mock.ExpectCommit().WillReturnError(rollbackReason)
			} else {
				mock.ExpectRollback()
			}
			var err error
			func() {
				defer func() {
					value := recover()
					if mode == "panic" {
						if value != "fixture panic" {
							t.Errorf("panic=%v", value)
						}
					} else if value != nil {
						t.Errorf("unexpected panic=%v", value)
					}
				}()
				err = d.Transaction(context.Background(), &sql.TxOptions{}, func(tx *gorm.DB) error {
					if tx.Statement.ConnPool == d.pool {
						t.Fatal("transaction callback escaped to root pool")
					}
					if err := tx.Exec("UPDATE fixture SET value = ? WHERE id = ?", "value", int64(1)).Error; err != nil {
						return err
					}
					if mode == "rollback" {
						return rollbackReason
					}
					if mode == "panic" {
						panic("fixture panic")
					}
					return nil
				})
			}()
			if mode == "commit" && err != nil {
				t.Fatal(err)
			}
			if (mode == "rollback" || mode == "commit failure") && !errors.Is(err, rollbackReason) {
				t.Fatalf("error chain=%v", err)
			}
		})
	}
}

func TestSchemaReadOnlyChecks(t *testing.T) {
	for _, tc := range []struct {
		name     string
		table    bool
		rows     [][]any
		required int64
		want     error
	}{
		{name: "no migrations yet"},
		{name: "missing required migration", required: 1, want: ErrSchemaMismatch},
		{name: "empty version table", table: true},
		{name: "matching version", table: true, rows: [][]any{{int64(2), false}}, required: 2},
		{name: "different version", table: true, rows: [][]any{{int64(1), false}}, required: 2, want: ErrSchemaMismatch},
		{name: "dirty version", table: true, rows: [][]any{{int64(2), true}}, required: 2, want: ErrSchemaDirty},
		{name: "multiple version rows", table: true, rows: [][]any{{int64(2), false}, {int64(2), false}}, required: 2, want: ErrSchemaMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, mock := mockDatabase(t)
			var table any
			if tc.table {
				table = "schema_migrations"
			}
			mock.ExpectQuery("SELECT to_regclass").WillReturnRows(sqlmock.NewRows([]string{"table"}).AddRow(table))
			if tc.table {
				rows := sqlmock.NewRows([]string{"version", "dirty"})
				for _, row := range tc.rows {
					rows.AddRow(row[0], row[1])
				}
				mock.ExpectQuery("SELECT version, dirty").WillReturnRows(rows)
			}
			err := d.CheckSchema(context.Background(), tc.required)
			if !errors.Is(err, tc.want) {
				t.Fatalf("schema check=%v want=%v", err, tc.want)
			}
		})
	}
}

func TestSessionErrorChainAndCancellation(t *testing.T) {
	d, mock := mockDatabase(t)
	source := &pgconn.PgError{Code: "23505", Message: "private-data", Detail: "private-data"}
	mock.ExpectExec("INSERT INTO fixture").WillReturnError(source)
	err := d.WithSession(context.Background(), func(db *gorm.DB) error { return db.Exec("INSERT INTO fixture VALUES (?)", "private-data").Error })
	var pgError *pgconn.PgError
	if !errors.As(err, &pgError) || strings.Contains(err.Error(), "private-data") {
		t.Fatal("driver error exposed or chain lost")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	err = d.WithSession(ctx, func(*gorm.DB) error { called = true; return nil })
	if called || !errors.Is(err, context.Canceled) {
		t.Fatal("canceled session executed callback")
	}
}
