/**
 * @Author: LiuShuXin
 * @Description: 队列错误分类与防止载荷、异常文本泄漏的边界
 * @File: errors.go
 * Software: Goland
 * @Date: 2026/10/15
 */
package taskqueue

import (
	"context"
	"errors"
	"fmt"

	"github.com/cauliflower-beep/easy-go-framework/logging"
	"github.com/cauliflower-beep/easy-go-framework/redis"
	"github.com/hibiken/asynq"
)

var (
	ErrClosed     = errors.New("taskqueue is closed")
	ErrStarted    = errors.New("taskqueue worker already starting or started")
	ErrNotFound   = errors.New("taskqueue task not found")
	ErrIDConflict = errors.New("taskqueue task ID already exists")
	ErrSkipRetry  = errors.New("task must not be retried")
)

type safeError struct{ cause error }

func (e *safeError) Unwrap() error { return e.cause }
func (e *safeError) Is(target error) bool {
	if target == redis.ErrAuthentication || target == redis.ErrPermission {
		return errors.Is(redis.SafeError(e.cause), target)
	}
	if target == ErrNotFound {
		return errors.Is(e.cause, asynq.ErrTaskNotFound) || errors.Is(e.cause, asynq.ErrQueueNotFound)
	}
	if target == ErrIDConflict {
		return errors.Is(e.cause, asynq.ErrTaskIDConflict)
	}
	return false
}
func (e *safeError) Error() string {
	for _, kind := range []error{ErrClosed, ErrNotFound, ErrIDConflict, ErrStarted, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(e, kind) {
			return kind.Error()
		}
	}
	return "taskqueue operation failed"
}
func (e *safeError) Format(s fmt.State, _ rune) { _, _ = fmt.Fprint(s, e.Error()) }
func (e *safeError) LogDiagnostic() logging.Diagnostic {
	// 复用 Redis 的认证、TLS 等安全诊断，但不展开 Asynq 包装文本。
	if e.Error() == "taskqueue operation failed" {
		return redis.SafeError(e.cause).(logging.DiagnosticProvider).LogDiagnostic()
	}
	return logging.Diagnostic{Message: e.Error()}
}
func SafeError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := err.(*safeError); ok {
		return err
	}
	return &safeError{cause: err}
}

type permanentError struct{ cause error }

func (e *permanentError) Error() string        { return "task handler failed permanently" }
func (e *permanentError) Unwrap() error        { return e.cause }
func (e *permanentError) Is(target error) bool { return target == ErrSkipRetry }

func (e *permanentError) Format(s fmt.State, _ rune) { _, _ = fmt.Fprint(s, e.Error()) }
func (e *permanentError) LogDiagnostic() logging.Diagnostic {
	return logging.Diagnostic{Message: e.Error()}
}

// Permanent 明确本次失败不再自动重试，不撤销已经发生的业务副作用。
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &permanentError{cause: err}
}
