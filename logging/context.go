/**
 * @Author: LiuShuXin
 * @Description: 通过标准 Context 传递请求标识并派生业务日志
 * @File: context.go
 * Software: Goland
 * @Date: 2026/10/10 11:10
 */

package logging

import (
	"context"

	"go.uber.org/zap"
)

type requestIDKey struct{}

// WithRequestID 使用私有键，避免与其他组件的 Context 字段冲突。
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestID 返回当前请求标识；非请求上下文返回空字符串。
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// ForRequest 派生请求日志，保留模块字段和原输出，不修改共享日志对象。
func ForRequest(logger *zap.Logger, ctx context.Context) *zap.Logger {
	if id := RequestID(ctx); id != "" {
		return logger.With(zap.String("request_id", id))
	}
	return logger
}
