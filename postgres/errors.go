/**
 * @Author: LiuShuXin
 * @Description: 保留数据库错误链，同时对外输出稳定且不含参数的诊断
 * @File: errors.go
 * Software: Goland
 * @Date: 2026/10/12
 */
package postgres

import (
	"context"
	"crypto/x509"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"

	"github.com/cauliflower-beep/easy-go-framework/logging"
)

var ErrClosed = errors.New("database is closed")

var ErrAmbientService = errors.New("PGSERVICE is unsupported; use application PostgreSQL configuration")

type safeError struct{ cause error }

func (e *safeError) Error() string {
	switch {
	case errors.Is(e.cause, ErrClosed):
		return ErrClosed.Error()
	case errors.Is(e.cause, ErrAmbientService):
		return ErrAmbientService.Error()
	case errors.Is(e.cause, ErrSchemaMismatch):
		return ErrSchemaMismatch.Error()
	case errors.Is(e.cause, ErrSchemaDirty):
		return ErrSchemaDirty.Error()
	case errors.Is(e.cause, context.DeadlineExceeded):
		return "database operation timed out"
	case errors.Is(e.cause, context.Canceled):
		return "database operation canceled"
	case errors.Is(e.cause, gorm.ErrRecordNotFound):
		return "database record not found"
	case errors.Is(e.cause, sql.ErrConnDone):
		return "database connection closed"
	}
	var pgError *pgconn.PgError
	if errors.As(e.cause, &pgError) && pgError != nil {
		if validSQLState(pgError.Code) {
			return fmt.Sprintf("PostgreSQL operation failed (SQLSTATE %s)", pgError.Code)
		}
		return "PostgreSQL operation failed"
	}
	return "database operation failed"
}
func (e *safeError) Unwrap() error { return e.cause }

// SafeError 用于日志和返回诊断；errors.Is/As 仍可识别原始数据库错误。
// 禁止在日志中继续展开 Unwrap 后的消息或 PgError.Detail。
func SafeError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := err.(*safeError); ok {
		return err
	}
	return &safeError{cause: err}
}

// GoString 避免调试格式展开错误链中的敏感值。
func (e *safeError) GoString() string { return e.Error() }

// LogDiagnostic 提供网络原因、SQLSTATE 和受控对象名称，不展开服务端错误正文或连接配置。
// Error 仍保留稳定安全消息；内部日志单独使用此诊断接口。
func (e *safeError) LogDiagnostic() logging.Diagnostic {
	result := logging.Diagnostic{Message: e.Error()}
	var pgError *pgconn.PgError
	if errors.As(e.cause, &pgError) && pgError != nil {
		if validSQLState(pgError.Code) {
			result.SQLState = pgError.Code
		}
		result.Schema = diagnosticIdentifier(pgError.SchemaName)
		result.Table = diagnosticIdentifier(pgError.TableName)
		result.Constraint = diagnosticIdentifier(pgError.ConstraintName)
		return result
	}
	var network *net.OpError
	if errors.As(e.cause, &network) {
		result.Operation = network.Op
		if network.Addr != nil {
			result.Target = network.Addr.String()
		}
		if network.Timeout() {
			result.Message = "database network operation timed out"
		}
	}
	// 只提取已知类型的安全原因；未知驱动或回调错误可能带数据，不能在此直接展开。
	var errno syscall.Errno
	if errors.As(e.cause, &errno) {
		result.Message += "; " + errno.Error()
		return result
	}
	var dns *net.DNSError
	if errors.As(e.cause, &dns) {
		result.Operation, result.Target = "lookup", dns.Name
		switch {
		case dns.IsTimeout:
			result.Message = "database DNS lookup timed out"
		case dns.IsNotFound:
			result.Message = "database DNS name not found"
		default:
			result.Message = "database DNS lookup failed"
		}
		return result
	}
	var hostname x509.HostnameError
	var authority x509.UnknownAuthorityError
	var certificate x509.CertificateInvalidError
	switch {
	case errors.As(e.cause, &hostname):
		result.Message = "database TLS certificate hostname mismatch"
	case errors.As(e.cause, &authority):
		result.Message = "database TLS certificate authority is not trusted"
	case errors.As(e.cause, &certificate):
		result.Message = "database TLS certificate is invalid"
	case errors.Is(e.cause, fs.ErrNotExist):
		result.Message = "database configuration file not found"
	case errors.Is(e.cause, io.EOF):
		result.Message = "database connection ended unexpectedly"
	}
	return result
}

func validSQLState(code string) bool {
	return len(code) == 5 && strings.Trim(code, "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ") == ""
}

// DDL 名称须由开发者定义，不拼接业务输入；诊断仅接受短 ASCII 标识符。
// 自定义 RAISE 可填写任意文本，非法或过长名称直接省略，不截断为歧义名称。
func diagnosticIdentifier(name string) string {
	if len(name) == 0 || len(name) > 63 {
		return ""
	}
	for i := range len(name) {
		ch := name[i]
		if ch == '_' || ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || i > 0 && (ch >= '0' && ch <= '9' || ch == '$') {
			continue
		}
		return ""
	}
	return name
}
