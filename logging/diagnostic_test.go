/**
 * @Author: LiuShuXin
 * @Description: 验证真实错误原因、敏感边界、凭证清理与诊断数量上限
 * @File: diagnostic_test.go
 * Software: Goland
 * @Date: 2026/10/13
 */
package logging_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"unicode/utf8"

	"github.com/cauliflower-beep/easy-go-framework/logging"
)

type protectedDiagnostic struct{ cause error }

func (e protectedDiagnostic) Error() string { return e.cause.Error() }
func (e protectedDiagnostic) Unwrap() error { return e.cause }
func (protectedDiagnostic) LogDiagnostic() logging.Diagnostic {
	return logging.Diagnostic{Message: "credential rejected"}
}

func TestDiagnosticsPreserveCausesAndSensitiveBoundary(t *testing.T) {
	source := errors.Join(errors.New("encoder allocation failed"), &net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 5432}, Err: syscall.ECONNREFUSED})
	causes, truncated := logging.Diagnostics(fmt.Errorf("operation: %w", source))
	encoded, _ := json.Marshal(causes)
	if truncated || !strings.Contains(string(encoded), "encoder allocation failed") || !strings.Contains(string(encoded), "192.0.2.1:5432") || !strings.Contains(string(encoded), "dial") {
		t.Fatalf("diagnostics=%s truncated=%v", encoded, truncated)
	}
	secret := errors.New("unlabeled-sensitive-input")
	causes, truncated = logging.Diagnostics(fmt.Errorf("operation: %w", protectedDiagnostic{cause: secret}))
	encoded, _ = json.Marshal(causes)
	if truncated || strings.Contains(string(encoded), secret.Error()) || !strings.Contains(string(encoded), "credential rejected") {
		t.Fatalf("boundary failed: %s", encoded)
	}
}

func TestDiagnosticCredentialCleanupAndURLContext(t *testing.T) {
	for _, text := range []string{
		`password="fixture secret with spaces" token=fixture-token`,
		`password='fixture secret with spaces' api_key=fixture-key`,
		`{"password":"fixture-password","token":"fixture-token"}`,
		`Authorization: Bearer fixture-token`,
		`authorization: Basic Zml4dHVyZTpzZWNyZXQ=`,
		`connection postgres://fixture-user:fixture-pass@192.0.2.1/database?password=fixture-query`,
		`password="` + strings.Repeat("fixture-secret ", 400),
	} {
		causes, _ := logging.Diagnostics(errors.New(text))
		encoded, _ := json.Marshal(causes)
		if strings.Contains(string(encoded), "fixture") || strings.Contains(string(encoded), "Zml4dHVyZTpzZWNyZXQ=") {
			t.Fatalf("credential leaked: %s", encoded)
		}
	}
	err := &url.Error{Op: "Get", URL: "https://user:fixture-password@api.example/endpoint?token=fixture-token#fixture-fragment", Err: errors.New("upstream connection closed")}
	causes, _ := logging.Diagnostics(err)
	encoded, _ := json.Marshal(causes)
	if strings.Contains(string(encoded), "fixture") || !strings.Contains(string(encoded), "api.example/endpoint") || !strings.Contains(string(encoded), "upstream connection closed") {
		t.Fatalf("unsafe or incomplete URL diagnostic: %s", encoded)
	}
}

type cyclicDiagnostic struct{}

func (*cyclicDiagnostic) Error() string   { panic("unexpected cycle formatting") }
func (e *cyclicDiagnostic) Unwrap() error { return e }

type brokenDiagnostic struct{}

func (brokenDiagnostic) Error() string { panic("formatter panic") }

func TestDiagnosticsAreBoundedAndRobust(t *testing.T) {
	causes, truncated := logging.Diagnostics(&cyclicDiagnostic{})
	if !truncated || len(causes) != 8 {
		t.Fatal("cyclic cause chain unbounded")
	}
	causes, _ = logging.Diagnostics(brokenDiagnostic{})
	if len(causes) != 1 || causes[0].Message != "error diagnostic failed" {
		t.Fatal("bad formatter escaped diagnostic handler")
	}
	causes, _ = logging.Diagnostics(errors.New(strings.Repeat("错误原因", 500)))
	if !utf8.ValidString(causes[0].Message) || len(causes[0].Message) > 1027 {
		t.Fatal("message truncation corrupts UTF-8 or exceeds bound")
	}
}
