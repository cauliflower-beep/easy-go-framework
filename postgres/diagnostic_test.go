/**
 * @Author: LiuShuXin
 * @Description: 验证数据库内部诊断保留网络原因并隔离 SQL 值和凭证
 * @File: diagnostic_test.go
 * Software: Goland
 * @Date: 2026/10/13
 */
package postgres

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"syscall"
	"testing"

	"github.com/cauliflower-beep/easy-go-framework/logging"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestPostgresDiagnosticRetainsNetworkCause(t *testing.T) {
	source := &net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 5432}, Err: syscall.ECONNREFUSED}
	err := fmt.Errorf("database connect: %w", SafeError(source))
	causes, _ := logging.Diagnostics(err)
	encoded, _ := json.Marshal(causes)
	if !strings.Contains(string(encoded), "192.0.2.1:5432") || !strings.Contains(string(encoded), "dial") || !strings.Contains(string(encoded), syscall.ECONNREFUSED.Error()) {
		t.Fatalf("network cause lost: %s", encoded)
	}
}

func TestPostgresDiagnosticNeverExpandsServerPayload(t *testing.T) {
	source := &pgconn.PgError{Code: "23505", Message: "private-bind-value", Detail: "private-password", InternalQuery: "private-SQL"}
	for _, err := range []error{source, SafeError(source), fmt.Errorf("operation: %w", source), fmt.Errorf("operation: %w", SafeError(source))} {
		causes, _ := logging.Diagnostics(err)
		encoded, _ := json.Marshal(causes)
		if strings.Contains(string(encoded), "private") || !strings.Contains(string(encoded), "23505") {
			t.Fatalf("unsafe database diagnostics: %s", encoded)
		}
	}
}

func TestPostgresDiagnosticIncludesControlledObjectNames(t *testing.T) {
	source := &pgconn.PgError{
		Code: "23505", SchemaName: "public", TableName: "fixture_accounts", ConstraintName: "fixture_accounts_email_key",
		Message: "private-bind-value", Detail: "private-email", InternalQuery: "private-SQL",
	}
	for _, err := range []error{SafeError(source), fmt.Errorf("operation: %w", SafeError(source))} {
		causes, truncated := logging.Diagnostics(err)
		last := causes[len(causes)-1]
		encoded, _ := json.Marshal(causes)
		if truncated || last.SQLState != "23505" || last.Schema != "public" || last.Table != "fixture_accounts" || last.Constraint != "fixture_accounts_email_key" || last.Target != "" || strings.Contains(string(encoded), "private") {
			t.Fatalf("unsafe or missing schema metadata: %s", encoded)
		}
	}
}

func TestPostgresDiagnosticRejectsFreeTextMetadata(t *testing.T) {
	for _, name := range []string{"private@email.example", "password=private", "object\nprivate", "object\x1b[31mprivate", "quoted object", "非受控名称", strings.Repeat("a", 64)} {
		source := &pgconn.PgError{Code: "23505", SchemaName: name, TableName: "fixture_table", ConstraintName: name}
		causes, _ := logging.Diagnostics(SafeError(source))
		if len(causes) != 1 || causes[0].Schema != "" || causes[0].Constraint != "" || causes[0].Table != "fixture_table" || causes[0].SQLState != "23505" {
			t.Fatalf("uncontrolled metadata accepted: %+v", causes)
		}
	}
	source := &pgconn.PgError{Code: "private-code", ConstraintName: strings.Repeat("a", 63)}
	causes, _ := logging.Diagnostics(SafeError(source))
	if len(causes) != 1 || causes[0].SQLState != "" || len(causes[0].Constraint) != 63 || strings.Contains(SafeError(source).Error(), "private") {
		t.Fatal("invalid SQLSTATE leaked or bounded metadata was discarded")
	}
}
