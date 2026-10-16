/**
 * @Author: LiuShuXin
 * @Description: 调度器状态错误与安全函数失败分类
 * @File: errors.go
 * Software: Goland
 * @Date: 2026/10/15
 */
package scheduler

import (
	"context"
	"errors"
	"fmt"

	"github.com/cauliflower-beep/easy-go-framework/logging"
)

var (
	ErrClosed    = errors.New("scheduler is closed")
	ErrStarted   = errors.New("scheduler already started")
	ErrDuplicate = errors.New("scheduler plan name already registered")
	ErrNotFound  = errors.New("scheduler plan not found")
	errPanic     = errors.New("scheduler handler panicked")
)

type safeError struct{ cause error }

func (e *safeError) Unwrap() error { return e.cause }
func (e *safeError) Error() string {
	for _, target := range []error{ErrClosed, ErrStarted, ErrDuplicate, ErrNotFound, errPanic, context.DeadlineExceeded, context.Canceled} {
		if errors.Is(e.cause, target) {
			return target.Error()
		}
	}
	return "scheduler operation failed"
}
func (e *safeError) Format(s fmt.State, _ rune) { _, _ = fmt.Fprint(s, e.Error()) }
func (e *safeError) LogDiagnostic() logging.Diagnostic {
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

func failureReason(err error) string {
	switch {
	case errors.Is(err, errPanic):
		return "handler_panicked"
	case errors.Is(err, context.DeadlineExceeded):
		return "handler_deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return "handler_canceled"
	default:
		return "handler_failed"
	}
}
