/**
 * @Author: LiuShuXin
 * @Description: 统一 JSON API 响应并防止序列化失败后提交错误状态
 * @File: response.go
 * Software: Goland
 * @Date: 2026/10/13
 */

// Package httpapi 提供 JSON API 的响应、错误适配和参数校验。
// 下载、流式接口、204 响应及健康探针可直接使用 HTTP 原生响应。
package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/cauliflower-beep/easy-go-framework/apperror"
	"github.com/cauliflower-beep/easy-go-framework/logging"
)

// SuccessResponse 保留真实 HTTP 状态；Data 在成功响应中始终存在，包括 null。
type SuccessResponse struct {
	Code      apperror.Code `json:"code"`
	Message   string        `json:"message"`
	Data      any           `json:"data"`
	RequestID string        `json:"request_id"`
}

// ErrorResponse 不包含成功数据；Details 仅用于公开校验说明。
type ErrorResponse struct {
	Code      apperror.Code `json:"code"`
	Message   string        `json:"message"`
	RequestID string        `json:"request_id"`
	Details   []Violation   `json:"details,omitempty"`
}

// Violation 只包含公开字段和规则，不包含输入值或校验器原始消息。
type Violation struct {
	Field string `json:"field"`
	Rule  string `json:"rule"`
}

var ErrResponseCommitted = errors.New("HTTP response already committed")

// Success 是常规 200 JSON 响应；其他有正文的成功状态使用 Respond。
func Success(c *gin.Context, data any) error { return Respond(c, http.StatusOK, data) }

func Respond(c *gin.Context, status int, data any) error {
	if status < 200 || status >= 300 || status == http.StatusNoContent || status == http.StatusResetContent {
		return errors.New("invalid JSON success status")
	}
	return writeJSON(c, status, SuccessResponse{Code: apperror.CodeOK, Message: "success", Data: data, RequestID: RequestID(c)})
}

// OK 返回没有业务数据的 200 JSON 响应；无正文的 204 使用 HTTP 原生响应。
func OK(c *gin.Context) error { return Success(c, nil) }

// RequestID 获取框架已分配的标识，与响应头及请求日志一致。
func RequestID(c *gin.Context) string { return logging.RequestID(c.Request.Context()) }

// Abort 为协议层拒绝提供相同的错误结构；业务错误由 ErrorHandler 中间件映射。
func Abort(c *gin.Context, status int, code apperror.Code, message string) {
	c.Abort()
	if c.Writer.Written() {
		return
	}
	if status < 400 || status > 599 || code <= 0 {
		status, code, message = http.StatusInternalServerError, apperror.CodeInternalError, "internal server error"
	}
	response := ErrorResponse{Code: code, Message: message, RequestID: RequestID(c)}
	recordOutcome(c, response)
	// 此处仅序列化框架定义的字符串，不接收业务序列化对象。
	if err := writeJSON(c, status, response); err != nil {
		_ = c.Error(err)
	}
}

func writeJSON(c *gin.Context, status int, response any) error {
	if c.Writer.Written() {
		return ErrResponseCommitted
	}
	// 在写状态和正文之前完成编码；失败时错误中间件仍能安全返回 500。
	payload, err := json.Marshal(response)
	if err != nil {
		return err
	}
	c.Header("Content-Type", "application/json; charset=utf-8")
	c.Writer.WriteHeader(status)
	_, err = c.Writer.Write(payload)
	return err
}
