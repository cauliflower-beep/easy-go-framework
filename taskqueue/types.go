/**
 * @Author: LiuShuXin
 * @Description: 与队列引擎解耦的任务、状态、处理函数及版本化载荷
 * @File: types.go
 * Software: Goland
 * @Date: 2026/10/15
 */
package taskqueue

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
)

// Submission 的 Context 仅控制提交，不控制后台任务寿命。
// ID 可留空自动生成；固定 UUID 可用于核实提交结果未知的请求。
// MaxRetry 为额外尝试次数，零表示普通处理失败后不自动重试。
type Submission struct {
	ID       string
	Type     string
	Payload  json.RawMessage
	Delay    time.Duration
	Timeout  time.Duration
	MaxRetry int
}

type State string

const (
	StatePending   State = "pending"
	StateScheduled State = "scheduled"
	StateActive    State = "active"
	StateRetry     State = "retry"
	StateFailed    State = "failed"
	StateCompleted State = "completed"
)

// Info 只描述执行事实；Result 仅在 completed 时返回。它不是可直接公开的业务 DTO。
// 成功结果超过保留期或被清理后返回 ErrNotFound，不能解释为从未执行。
type Info struct {
	ID            string
	Type          string
	State         State
	MaxRetry      int
	Retried       int
	LastFailure   string
	NextProcessAt time.Time
	LastFailedAt  time.Time
	CompletedAt   time.Time
	Result        json.RawMessage
}

// Task 的 Payload 是本次尝试的独立数据。Attempt=Retried+1，不是全局唯一执行次数。
type Task struct {
	ID       string
	Type     string
	Payload  json.RawMessage
	Attempt  int
	MaxRetry int
}
type Handler func(context.Context, Task) (json.RawMessage, error)

// Data 使用 base64 保存 JSON 原始字节，避免 JSON 转义改变载荷大小和内容。
type envelope struct {
	Version   int    `json:"version"`
	RequestID string `json:"request_id,omitempty"`
	Data      []byte `json:"data"`
}

func (t Submission) String() string   { return "taskqueue.Submission{Payload:[REDACTED]}" }
func (t Submission) GoString() string { return t.String() }
func (t Task) String() string         { return "taskqueue.Task{Payload:[REDACTED]}" }
func (t Task) GoString() string       { return t.String() }
func (t Info) String() string         { return "taskqueue.Info{Result:[REDACTED]}" }
func (t Info) GoString() string       { return t.String() }

func validID(id string) bool {
	u, err := uuid.Parse(id)
	return err == nil && u != uuid.Nil && u.String() == id
}
func snapshot(t *asynq.TaskInfo) Info {
	state := map[asynq.TaskState]State{asynq.TaskStatePending: StatePending, asynq.TaskStateScheduled: StateScheduled, asynq.TaskStateActive: StateActive, asynq.TaskStateRetry: StateRetry, asynq.TaskStateArchived: StateFailed, asynq.TaskStateCompleted: StateCompleted}[t.State]
	out := Info{ID: t.ID, Type: t.Type, State: state, MaxRetry: t.MaxRetry, Retried: t.Retried, NextProcessAt: t.NextProcessAt, LastFailedAt: t.LastFailedAt, CompletedAt: t.CompletedAt}
	if t.LastErr != "" {
		out.LastFailure = failureLabel(t.LastErr)
	}
	if state == StateCompleted {
		out.Result = append(json.RawMessage(nil), t.Result...)
	}
	return out
}

func failureLabel(message string) string {
	switch message {
	case "task handler failed", "task handler panicked", "task handler not registered", "invalid task payload", "invalid task result", "task result exceeds limit", "task payload exceeds limit":
		return message
	case "context deadline exceeded":
		return "task deadline exceeded"
	case "context canceled":
		return "task execution canceled"
	default:
		return "task attempt failed"
	}
}
