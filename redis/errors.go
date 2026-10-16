/**
 * @Author: LiuShuXin
 * @Description: 区分 Redis 故障原因并阻止命令数据进入错误诊断
 * @File: errors.go
 * Software: Goland
 * @Date: 2026/10/14
 */
package redis

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"io/fs"
	"net"
	"strings"

	"github.com/cauliflower-beep/easy-go-framework/logging"
	goredis "github.com/redis/go-redis/v9"
)

// Nil 与 go-redis 的键不存在标识兼容；它不是连接失败。
var Nil = goredis.Nil
var ErrClosed = goredis.ErrClosed
var ErrPoolTimeout = goredis.ErrPoolTimeout
var ErrAuthentication = errors.New("Redis authentication failed")
var ErrPermission = errors.New("Redis permission denied")

type safeError struct{ cause error }

// SafeError 保留 errors.Is/As，Error 和日志诊断不展开键、值或脚本正文。
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
func (e *safeError) Is(target error) bool {
	if target == ErrAuthentication {
		return serverPrefix(e.cause, "WRONGPASS", "NOAUTH")
	}
	if target == ErrPermission {
		return serverPrefix(e.cause, "NOPERM")
	}
	return false
}
func serverPrefix(err error, prefixes ...string) bool {
	var server goredis.Error
	if !errors.As(err, &server) || server == nil {
		return false
	}
	code, _, _ := strings.Cut(server.Error(), " ")
	for _, prefix := range prefixes {
		if code == prefix {
			return true
		}
	}
	return false
}
func (e *safeError) Error() string {
	switch {
	case errors.Is(e.cause, ErrClosed):
		return "Redis client is closed"
	case errors.Is(e.cause, Nil):
		return "Redis key or value does not exist"
	case errors.Is(e.cause, ErrPoolTimeout):
		return "Redis connection pool wait timed out"
	case errors.Is(e.cause, context.Canceled):
		return "Redis operation canceled"
	case errors.Is(e.cause, context.DeadlineExceeded):
		return "Redis operation deadline exceeded"
	case serverPrefix(e.cause, "WRONGPASS", "NOAUTH"):
		return ErrAuthentication.Error()
	case serverPrefix(e.cause, "NOPERM"):
		return ErrPermission.Error()
	}
	var network net.Error
	if errors.As(e.cause, &network) && network.Timeout() {
		return "Redis network operation timed out"
	}
	var server goredis.Error
	if errors.As(e.cause, &server) {
		return "Redis server rejected operation"
	}
	return "Redis operation failed"
}
func (e *safeError) GoString() string { return e.Error() }
func (e *safeError) LogDiagnostic() logging.Diagnostic {
	result := logging.Diagnostic{Message: e.Error()}
	var network *net.OpError
	if errors.As(e.cause, &network) {
		result.Operation = network.Op
		// 地址可能来自非标准回调错误；组件诊断不输出原始 Target。
	}
	var hostname x509.HostnameError
	var authority x509.UnknownAuthorityError
	var certificate x509.CertificateInvalidError
	switch {
	case errors.As(e.cause, &hostname):
		result.Message = "Redis TLS certificate hostname mismatch"
	case errors.As(e.cause, &authority):
		result.Message = "Redis TLS certificate authority is not trusted"
	case errors.As(e.cause, &certificate):
		result.Message = "Redis TLS certificate is invalid"
	case errors.Is(e.cause, fs.ErrNotExist):
		result.Message = "Redis configuration file not found"
	case errors.Is(e.cause, io.EOF):
		result.Message = "Redis connection ended unexpectedly"
	}
	return result
}
