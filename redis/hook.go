/**
 * @Author: LiuShuXin
 * @Description: 将 Redis 命令及 Pipeline 诊断接入日志并保护返回错误
 * @File: hook.go
 * Software: Goland
 * @Date: 2026/10/14
 */
package redis

import (
	"context"
	"errors"
	"net"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

type commandHook struct {
	logger *zap.Logger
	slow   time.Duration
}

func (h commandHook) DialHook(next goredis.DialHook) goredis.DialHook {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := next(ctx, network, address)
		return conn, SafeError(commandError(ctx, err))
	}
}
func (h commandHook) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error {
		start := time.Now()
		err := next(ctx, cmd)
		// 只有发生实际失败时合并取消，避免把已确认成功的写操作改为失败。
		err = commandError(ctx, err)
		logCommandOutcome(h.logger, ctx, err, time.Since(start), h.slow, []goredis.Cmder{cmd})
		return SafeError(err)
	}
}
func (h commandHook) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []goredis.Cmder) error {
		start := time.Now()
		err := next(ctx, cmds)
		// Pipeline 的各命令 Result() 也必须经过安全边界，不能只保护 Exec 返回值。
		diagnosticErr := err
		for _, cmd := range cmds {
			if commandErr := cmd.Err(); commandErr != nil && !errors.Is(commandErr, Nil) && (diagnosticErr == nil || errors.Is(diagnosticErr, Nil)) {
				diagnosticErr = commandErr
			}
			cmd.SetErr(SafeError(commandError(ctx, cmd.Err())))
		}
		err = commandError(ctx, err)
		logCommandOutcome(h.logger, ctx, commandError(ctx, diagnosticErr), time.Since(start), h.slow, cmds)
		return SafeError(err)
	}
}

// 驱动把 Context 的绝对期限设置到 socket；网络轮询可能先于 Context
// timer 的取消通知返回。不能仅依赖 ctx.Err()，也不能把较早的读写超时
// 一律解释成请求期限。只在失败为网络超时且请求期限已过时补齐分类。
func commandError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	canceled := ctx.Err()
	if canceled == nil {
		var network net.Error
		if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) && errors.As(err, &network) && network.Timeout() {
			canceled = context.DeadlineExceeded
		}
	}
	return errors.Join(err, canceled)
}
