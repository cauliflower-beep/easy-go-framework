/**
 * @Author: LiuShuXin
 * @Description: 将独立定时调度与 Redis 持久化任务队列显式组合
 * @File: queue.go
 * Software: Goland
 * @Date: 2026/10/15
 */

// Package queue 是可选的 scheduler/taskqueue 适配层；仅使用 scheduler 时无需导入。
package queue

import (
	"context"
	"errors"
	"time"

	"github.com/cauliflower-beep/easy-go-framework/scheduler"
	"github.com/cauliflower-beep/easy-go-framework/taskqueue"
)

// Plan 的 Schedule.Timeout 限制入队调用，TaskTimeout 限制 Worker 业务执行。
// Payload 是 JSON 文本；空值按 taskqueue 的 JSON null 处理，不提供模板替换。
type Plan struct {
	Schedule    scheduler.Plan `mapstructure:"schedule"`
	TaskType    string         `mapstructure:"task_type"`
	Payload     string         `mapstructure:"payload"`
	TaskTimeout time.Duration  `mapstructure:"task_timeout"`
	MaxRetry    int            `mapstructure:"max_retry"`
}

func (p Plan) String() string   { return "queue.Plan{Payload:[REDACTED]}" }
func (p Plan) GoString() string { return p.String() }

// Register 在启动前校验模板并绑定入队函数，不连接 Redis、不生成任务 ID。
// 不关闭借用的 Client；本次运行成功仅表示入队成功，不等待 Worker。
// Entry.LastRunID 就是队列任务 ID，入队超时后仍可用它查询未知的提交结果。
func Register(s *scheduler.Scheduler, client *taskqueue.Client, p Plan) error {
	if s == nil || client == nil {
		return errors.New("scheduler queue adapter requires scheduler and taskqueue client")
	}
	template := taskqueue.Submission{Type: p.TaskType, Payload: []byte(p.Payload), Timeout: p.TaskTimeout, MaxRetry: p.MaxRetry}
	if err := client.ValidateSubmission(template); err != nil {
		return err
	}
	return s.Register(p.Schedule, func(ctx context.Context) error {
		in := template
		in.ID = scheduler.RunID(ctx)
		_, err := client.Enqueue(ctx, in)
		return err
	})
}
