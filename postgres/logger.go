/**
 * @Author: LiuShuXin
 * @Description: 将 GORM 查询诊断接入模块日志，避免展开 SQL 和绑定参数
 * @File: logger.go
 * Software: Goland
 * @Date: 2026/10/12
 */
package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/cauliflower-beep/easy-go-framework/logging"
	"go.uber.org/zap"
	"gorm.io/gorm"
	gormlog "gorm.io/gorm/logger"
)

type queryLogger struct {
	logger *zap.Logger
	slow   time.Duration
	level  gormlog.LogLevel
}

func (l queryLogger) LogMode(level gormlog.LogLevel) gormlog.Interface { l.level = level; return l }
func (l queryLogger) Info(ctx context.Context, _ string, _ ...any) {
	if l.level >= gormlog.Info {
		logging.ForRequest(l.logger, ctx).Debug("database diagnostic")
	}
}
func (l queryLogger) Warn(ctx context.Context, _ string, _ ...any) {
	if l.level >= gormlog.Warn {
		logging.ForRequest(l.logger, ctx).Warn("database diagnostic")
	}
}
func (l queryLogger) Error(ctx context.Context, _ string, _ ...any) {
	if l.level >= gormlog.Error {
		logging.ForRequest(l.logger, ctx).Error("database diagnostic")
	}
}
func (l queryLogger) Trace(ctx context.Context, begin time.Time, _ func() (string, int64), err error) {
	if l.level == gormlog.Silent {
		return
	}
	elapsed := time.Since(begin)
	logger := logging.ForRequest(l.logger, ctx)
	fields := []zap.Field{zap.Duration("duration", elapsed)}
	// 不调用 SQL 格式化回调，避免参数被插值后进入诊断链。
	switch {
	case err != nil && !errors.Is(err, gorm.ErrRecordNotFound) && l.level >= gormlog.Error:
		diagnostics, truncated := logging.Diagnostics(SafeError(err))
		logger.Error("database query failed", append(fields, zap.Error(SafeError(err)), zap.Any("causes", diagnostics), zap.Bool("causes_truncated", truncated))...)
	case l.slow > 0 && elapsed >= l.slow && l.level >= gormlog.Warn:
		logger.Warn("database query slow", fields...)
	case l.level >= gormlog.Info:
		logger.Debug("database query completed", fields...)
	}
}
