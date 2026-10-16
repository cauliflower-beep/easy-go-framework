/**
 * @Author: LiuShuXin
 * @Description: 定义与 HTTP 无关的应用错误分类和安全公开消息
 * @File: error.go
 * Software: Goland
 * @Date: 2026/10/13
 */

// Package apperror 定义应用错误，不依赖传输协议或数据库实现。
package apperror

import "strconv"

// Kind 表达错误语义，由各传输适配层决定状态码。
type Kind uint8

const (
	Internal Kind = iota
	InvalidArgument
	Unauthenticated
	Forbidden
	NotFound
	Conflict
	Unavailable
)

// Error 的 code/message 必须是开发者定义的公开内容，不拼接输入或内部诊断。
// 字段不可变，错误可作为共享哨兵；底层原因通过 errors.Is/As 识别。
type Error struct {
	kind    Kind
	code    Code
	message string
	cause   error
}

func New(kind Kind, code Code, message string) *Error { return Wrap(kind, code, message, nil) }

// Wrap 保存原始原因，但格式化错误时不展开原因。
func Wrap(kind Kind, code Code, message string, cause error) *Error {
	return &Error{kind: kind, code: code, message: message, cause: cause}
}

// nil 接收者安全可读、可展开；装入 error 接口后仍是错误，不能视为成功。
func (e *Error) Kind() Kind {
	if e == nil {
		return Internal
	}
	return e.kind
}

func (e *Error) Code() Code {
	if e == nil {
		return 0
	}
	return e.code
}

func (e *Error) Message() string {
	if e == nil {
		return ""
	}
	return e.message
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	return strconv.FormatInt(int64(e.code), 10) + ": " + e.message
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *Error) GoString() string { return e.Error() }
