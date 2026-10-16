/**
 * @Author: LiuShuXin
 * @Description: 只读核对运维 SQL 迁移版本，禁止应用启动时创建或迁移表
 * @File: schema.go
 * Software: Goland
 * @Date: 2026/10/12
 */
package postgres

import (
	"context"
	"database/sql"
	"errors"
)

var ErrSchemaMismatch = errors.New("database schema version does not match application")
var ErrSchemaDirty = errors.New("database schema migration is unfinished")

// CheckSchema 读取 golang-migrate 的 public.schema_migrations。
// required=0 允许当前尚无迁移表的空库；出现实际迁移后应用必须声明精确版本。
func (d *Database) CheckSchema(ctx context.Context, required int64) error {
	if ctx == nil || required < 0 {
		return errors.New("invalid schema check arguments")
	}
	work, cancel, scopeErr := d.scope(ctx, d.options.HealthTimeout)
	if scopeErr != nil {
		return SafeError(scopeErr)
	}
	defer cancel()
	var table sql.NullString
	if err := d.pool.QueryRowContext(work, "SELECT to_regclass('public.schema_migrations')::text").Scan(&table); err != nil {
		return SafeError(err)
	}
	if !table.Valid {
		if required == 0 {
			return nil
		}
		return ErrSchemaMismatch
	}
	rows, err := d.pool.QueryContext(work, "SELECT version, dirty FROM public.schema_migrations LIMIT 2")
	if err != nil {
		return SafeError(err)
	}
	defer rows.Close()
	var version int64
	var dirty bool
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return SafeError(err)
		}
		if required == 0 {
			return nil
		}
		return ErrSchemaMismatch
	}
	if err := rows.Scan(&version, &dirty); err != nil {
		return SafeError(err)
	}
	if rows.Next() {
		return ErrSchemaMismatch
	}
	if err := rows.Err(); err != nil {
		return SafeError(err)
	}
	if dirty {
		return ErrSchemaDirty
	}
	if version != required {
		return ErrSchemaMismatch
	}
	return nil
}
