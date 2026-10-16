/**
 * @Author: LiuShuXin
 * @Description: 调度函数异常与 panic 的诊断隔离验证
 * @File: diagnostic_test.go
 * Software: Goland
 * @Date: 2026/10/15
 */
package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cauliflower-beep/easy-go-framework/logging"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestHandlerFailureAndPanicPrivacy(t *testing.T) {
	for _, mode := range []string{"error", "panic"} {
		t.Run(mode, func(t *testing.T) {
			s := fixture(t)
			core, logs := observer.New(zap.DebugLevel)
			s.logger = zap.New(core)
			p := plan("private-test")
			registered := register(t, s, p)
			switch mode {
			case "error":
				registered.handler = func(context.Context) error {
					return errors.New("private-exception-value")
				}
			case "panic":
				registered.handler = func(context.Context) error { panic("private-panic-value") }
			}
			if err := s.Start(); err != nil {
				t.Fatal(err)
			}
			s.run(registered)
			entry, _ := s.Get(p.Name)
			if entry.LastOutcome != OutcomeFailed || entry.LastRunID == "" {
				t.Fatal("handler failure not recorded")
			}
			for _, log := range logs.All() {
				text := fmt.Sprint(log.Message, log.ContextMap())
				for _, secret := range []string{"private-payload-value", "private-exception-value", "private-panic-value"} {
					if strings.Contains(text, secret) {
						t.Fatal("private value logged")
					}
				}
			}
			encoded, _ := json.Marshal(entry)
			if strings.Contains(string(encoded), "private-payload-value") {
				t.Fatal("payload exposed in snapshot")
			}
		})
	}
}

func TestSafeErrorChainAndEngineLogger(t *testing.T) {
	for _, cause := range []error{ErrClosed, ErrStarted, ErrNotFound, ErrDuplicate, context.Canceled, context.DeadlineExceeded} {
		err := SafeError(fmt.Errorf("private-wrapper: %w", cause))
		if !errors.Is(err, cause) {
			t.Fatal("error chain lost")
		}
		if strings.Contains(fmt.Sprintf("%v %+v %#v", err, err, err), "private-wrapper") {
			t.Fatal("error format leaked")
		}
		entries, _ := logging.Diagnostics(err)
		if strings.Contains(fmt.Sprint(entries), "private-wrapper") {
			t.Fatal("diagnostic leaked")
		}
	}
	core, logs := observer.New(zap.DebugLevel)
	logger := engineLogger{zap.New(core)}
	logger.Info("private-info", "payload", "private-value")
	logger.Error(errors.New("private-error"), "private-message", "payload", "private-value")
	for _, entry := range logs.All() {
		if strings.Contains(fmt.Sprint(entry.Message, entry.ContextMap()), "private-") {
			t.Fatal("engine logger leaked")
		}
	}
}
