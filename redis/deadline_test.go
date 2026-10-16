/**
 * @Author: LiuShuXin
 * @Description: 验证套接字期限先于 Context 取消通知时的错误分类
 * @File: deadline_test.go
 * Software: Goland
 * @Date: 2026/10/15
 */
package redis

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// socket 的绝对期限可先于 Context timer 的通知被观察到。
// 固定这个时序，不依赖短 sleep 或调度器负载碰撞。
type pendingDeadlineContext struct {
	context.Context
	deadline time.Time
}

func (c pendingDeadlineContext) Deadline() (time.Time, bool) { return c.deadline, true }

func TestSocketDeadlineBeforeContextNotification(t *testing.T) {
	hook := commandHook{logger: zap.NewNop()}
	for _, expired := range []bool{false, true} {
		deadline := time.Now().Add(time.Hour)
		if expired {
			deadline = time.Now().Add(-time.Hour)
		}
		ctx := pendingDeadlineContext{Context: context.Background(), deadline: deadline}
		source := &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}
		cmd := goredis.NewStringCmd(ctx, "get", "private-key")
		err := hook.ProcessHook(func(context.Context, goredis.Cmder) error { return source })(ctx, cmd)
		if errors.Is(err, context.DeadlineExceeded) != expired {
			t.Fatalf("expired=%t: deadline classification lost: %v", expired, err)
		}
		var network *net.OpError
		if !errors.As(err, &network) || network != source {
			t.Fatal("original network error was lost")
		}
		cmd.SetErr(source)
		err = hook.ProcessPipelineHook(func(context.Context, []goredis.Cmder) error { return source })(ctx, []goredis.Cmder{cmd})
		if errors.Is(err, context.DeadlineExceeded) != expired || errors.Is(cmd.Err(), context.DeadlineExceeded) != expired {
			t.Fatalf("expired=%t: pipeline and command classifications differ: %v / %v", expired, err, cmd.Err())
		}
		if err := hook.ProcessHook(func(context.Context, goredis.Cmder) error { return nil })(ctx, cmd); err != nil {
			t.Fatal("confirmed successful command became a failure")
		}
	}
}
