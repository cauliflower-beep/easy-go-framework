/**
 * @Author: LiuShuXin
 * @Description: 有界记录错误原因链，支持组件安全诊断边界及常见凭证清理
 * @File: diagnostic.go
 * Software: Goland
 * @Date: 2026/10/13
 */
package logging

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"go.uber.org/zap"
)

// Diagnostic 是内部诊断数据，禁止直接放入客户端响应。
type Diagnostic struct {
	Type       string `json:"type"`
	Message    string `json:"message,omitempty"`
	Operation  string `json:"operation,omitempty"`
	Target     string `json:"target,omitempty"`
	SQLState   string `json:"sql_state,omitempty"`
	Schema     string `json:"schema,omitempty"`
	Table      string `json:"table,omitempty"`
	Constraint string `json:"constraint,omitempty"`
}

// DiagnosticProvider 由敏感数据来源实现；返回安全诊断后不再展开其原始原因。
// 普通内部错误保留叶子消息。通用清理无法识别所有业务秘密，敏感来源必须显式实现此边界。
type DiagnosticProvider interface{ LogDiagnostic() Diagnostic }

// ErrorFields 供组件和装配层复用同一套安全诊断，不直接打印原始错误对象。
func ErrorFields(err error) []zap.Field {
	causes, truncated := Diagnostics(err)
	return []zap.Field{zap.Any("causes", causes), zap.Bool("causes_truncated", truncated)}
}

var (
	secretPair    = regexp.MustCompile(`(?i)\b(password|passwd|pwd|token|secret|api[_-]?key)["']?\s*[:=]\s*("[^"]*"?|'[^']*'?|[^\s,;]+)`)
	authorization = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[a-z0-9+/=_\-.]+`)
	credentialURL = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^\s]+`)
)

// Diagnostics 保留包装顺序和 errors.Join 的各分支，最多记录 8 个节点。
// 不格式化普通包装节点，避免 fmt.Errorf 的消息重复展开敏感子错误。
func Diagnostics(err error) (result []Diagnostic, truncated bool) {
	var visit func(error)
	visit = func(current error) {
		if current == nil {
			return
		}
		if len(result) >= 8 {
			truncated = true
			return
		}
		entry, children := describe(current)
		entry.Type = fmt.Sprintf("%T", current)
		entry.Message = diagnosticText(entry.Message)
		entry.Operation = diagnosticText(entry.Operation)
		entry.Target = diagnosticText(entry.Target)
		entry.Schema = diagnosticText(entry.Schema)
		entry.Table = diagnosticText(entry.Table)
		entry.Constraint = diagnosticText(entry.Constraint)
		result = append(result, entry)
		for _, child := range children {
			visit(child)
			if truncated {
				break
			}
		}
	}
	visit(err)
	return
}

func describe(err error) (entry Diagnostic, children []error) {
	// 用户定义错误的格式化或 Unwrap 失败不应破坏原请求的错误响应。
	defer func() {
		if recover() != nil {
			entry = Diagnostic{Message: "error diagnostic failed"}
			children = nil
		}
	}()
	if provider, ok := err.(DiagnosticProvider); ok {
		return provider.LogDiagnostic(), nil
	}
	if state, ok := err.(interface{ SQLState() string }); ok {
		entry.Message = "database server rejected operation"
		code := state.SQLState()
		if len(code) == 5 && strings.Trim(code, "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ") == "" {
			entry.SQLState = code
		}
		return entry, nil
	}
	switch specific := err.(type) {
	case *net.OpError:
		entry.Message, entry.Operation = "network operation failed", specific.Op
		if specific.Addr != nil {
			entry.Target = specific.Addr.String()
		}
	case *url.Error:
		entry.Message, entry.Operation, entry.Target = "HTTP client operation failed", specific.Op, safeURL(specific.URL)
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		children = wrapped.Unwrap()
	case interface{ Unwrap() error }:
		if child := wrapped.Unwrap(); child != nil {
			children = []error{child}
		}
	}
	if len(children) == 0 && entry.Message == "" {
		entry.Message = err.Error()
	}
	return
}

func diagnosticText(value string) string {
	// 先限制输入规模；引号值允许截断到末尾，避免截断后的凭证漏过清理。
	value = limitText(value, 4096)
	value = credentialURL.ReplaceAllStringFunc(value, safeURL)
	value = secretPair.ReplaceAllString(value, "$1=[REDACTED]")
	value = authorization.ReplaceAllString(value, "$1 [REDACTED]")
	return limitText(value, 1024)
}

func safeURL(value string) string {
	u, err := url.Parse(value)
	if err != nil {
		return "[invalid URL]"
	}
	// 客户端 URL 的用户信息、查询串和片段可能包含凭证，诊断只保留定位地址。
	if u.User != nil {
		u.User = url.User("[REDACTED]")
	}
	u.RawQuery, u.Fragment = "", ""
	return u.String()
}

func limitText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value + "…"
}
