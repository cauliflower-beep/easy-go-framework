/**
 * @Author: LiuShuXin
 * @Description: 出站 HTTP 错误分类与敏感 URL、请求数据诊断边界
 * @File: errors.go
 * Software: Goland
 * @Date: 2026/10/14
 */
package httpclient

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"io/fs"
	"net"

	"github.com/cauliflower-beep/easy-go-framework/logging"
)

var (
	ErrClosed            = errors.New("HTTP client is closed")
	ErrResponseTooLarge  = errors.New("HTTP response body exceeds configured limit")
	ErrBodyClosed        = errors.New("HTTP response body is closed")
	ErrRedirectLimit     = errors.New("HTTP redirect limit exceeded")
	ErrRedirectOrigin    = errors.New("HTTP cross-origin redirect is not allowed")
	ErrRedirectDowngrade = errors.New("HTTPS to HTTP redirect is not allowed")
	ErrProtocolUpgrade   = errors.New("HTTP protocol upgrade is not supported")
)

type safeError struct{ cause error }

// SafeError 保留 errors.Is/As；直接格式化和框架日志不展开 URL、头或正文。
// 手动解包后的原始错误可能包含秘密，不应直接记录。
func SafeError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := err.(*safeError); ok {
		return err
	}
	return &safeError{cause: err}
}
func (e *safeError) Unwrap() error { return e.cause }
func (e *safeError) Error() string {
	for _, known := range []error{ErrClosed, ErrResponseTooLarge, ErrBodyClosed, ErrRedirectLimit, ErrRedirectOrigin, ErrRedirectDowngrade, ErrProtocolUpgrade} {
		if errors.Is(e.cause, known) {
			return known.Error()
		}
	}
	if errors.Is(e.cause, context.DeadlineExceeded) {
		return "HTTP request deadline exceeded"
	}
	if errors.Is(e.cause, context.Canceled) {
		return "HTTP request canceled"
	}
	var network net.Error
	if errors.As(e.cause, &network) && network.Timeout() {
		return "HTTP network operation timed out"
	}
	if errors.Is(e.cause, io.ErrUnexpectedEOF) || errors.Is(e.cause, io.EOF) {
		return "HTTP connection ended unexpectedly"
	}
	return "HTTP client operation failed"
}
func (e *safeError) GoString() string { return e.Error() }
func (e *safeError) LogDiagnostic() logging.Diagnostic {
	result := logging.Diagnostic{Message: e.Error()}
	var operation *net.OpError
	if errors.As(e.cause, &operation) {
		switch operation.Op {
		case "dial", "read", "write":
			result.Operation = operation.Op
		}
	}
	var hostname x509.HostnameError
	var authority x509.UnknownAuthorityError
	var certificate x509.CertificateInvalidError
	switch {
	case errors.As(e.cause, &hostname):
		result.Message = "HTTP TLS certificate hostname mismatch"
	case errors.As(e.cause, &authority):
		result.Message = "HTTP TLS certificate authority is not trusted"
	case errors.As(e.cause, &certificate):
		result.Message = "HTTP TLS certificate is invalid"
	case errors.Is(e.cause, fs.ErrNotExist):
		result.Message = "HTTP configuration file not found"
	}
	return result
}
