/**
 * @Author: LiuShuXin
 * @Description: 对象存储错误分类、错误链与安全诊断边界
 * @File: errors.go
 * Software: Goland
 * @Date: 2026/10/15
 */
package objectstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/cauliflower-beep/easy-go-framework/logging"
)

var (
	ErrClosed         = errors.New("objectstore client is closed")
	ErrNotFound       = errors.New("objectstore object not found")
	ErrBucketNotFound = errors.New("objectstore bucket not found")
	ErrPermission     = errors.New("objectstore access denied")
	ErrAuthentication = errors.New("objectstore authentication failed")
	ErrUnavailable    = errors.New("objectstore service unavailable")
	ErrBodyClosed     = errors.New("objectstore response body is closed")
)

type safeError struct {
	cause error
	kind  error
}

func (e *safeError) Error() string {
	if e.kind != nil {
		return e.kind.Error()
	}
	return "objectstore operation failed"
}
func (e *safeError) Unwrap() error                     { return e.cause }
func (e *safeError) Is(target error) bool              { return e.kind == target }
func (e *safeError) Format(s fmt.State, _ rune)        { _, _ = fmt.Fprint(s, e.Error()) }
func (e *safeError) LogDiagnostic() logging.Diagnostic { return logging.Diagnostic{Message: e.Error()} }

// SafeError 保留 errors.Is/As，但格式化不输出 key、URL、凭证或服务端消息。
// HEAD 的无错误码 404 只能说明资源不可用，不能证明是对象不存在。
func SafeError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := err.(*safeError); ok {
		return err
	}
	var code string
	var status int
	var oe *oss.ServiceError
	if errors.As(err, &oe) {
		code, status = oe.Code, oe.StatusCode
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		code = ae.ErrorCode()
	}
	var re *smithyhttp.ResponseError
	if errors.As(err, &re) {
		status = re.HTTPStatusCode()
	}
	var kind error
	switch code {
	case "NoSuchKey":
		kind = ErrNotFound
	case "NoSuchBucket":
		kind = ErrBucketNotFound
	case "InvalidAccessKeyId", "SignatureDoesNotMatch", "InvalidSecurityToken", "ExpiredToken", "SecurityTokenExpired":
		kind = ErrAuthentication
	case "AccessDenied":
		kind = ErrPermission
	case "SlowDown", "ServiceUnavailable", "InternalError", "RequestTimeout":
		kind = ErrUnavailable
	}
	if kind == nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			kind = context.DeadlineExceeded
		case errors.Is(err, context.Canceled):
			kind = context.Canceled
		case status == 401:
			kind = ErrAuthentication
		case status == 403:
			kind = ErrPermission
		case status == 429 || status >= 500:
			kind = ErrUnavailable
		}
	}
	for _, sentinel := range []error{ErrClosed, ErrBodyClosed, ErrNotFound, ErrBucketNotFound, ErrPermission, ErrAuthentication, ErrUnavailable} {
		if errors.Is(err, sentinel) {
			kind = sentinel
			break
		}
	}
	return &safeError{cause: err, kind: kind}
}
