/**
 * @Author: LiuShuXin
 * @Description: 将公开拒绝原因和多错误摘要交给统一访问日志
 * @File: completion.go
 * Software: Goland
 * @Date: 2026/10/13
 */
package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/cauliflower-beep/easy-go-framework/apperror"
)

const outcomeKey = "easy-framework.httpapi.outcome"
const summariesKey = "easy-framework.httpapi.errors"

type rejection struct {
	response ErrorResponse
}
type errorSummary struct {
	Index   int           `json:"index"`
	Status  int           `json:"status"`
	Code    apperror.Code `json:"code"`
	Message string        `json:"message"`
	Details []Violation   `json:"details,omitempty"`
}

func recordOutcome(c *gin.Context, response ErrorResponse) {
	c.Set(outcomeKey, rejection{response: response})
}

// Completion 由访问日志在 c.Next 之后调用，统一覆盖 Abort 和返回错误的拒绝路径。
// 只返回公开摘要；原始原因仅进入内部失败诊断日志。
func Completion(c *gin.Context) (zapcore.Level, []zap.Field) {
	level := zapcore.InfoLevel
	// 以最终响应为准，兼容原生 Gin 响应和已提交后才发生的错误。
	switch status := c.Writer.Status(); {
	case status >= 500 && status < 600:
		level = zapcore.ErrorLevel
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		level = zapcore.WarnLevel
	}
	var fields []zap.Field
	invalidState := false
	if value, ok := c.Get(outcomeKey); ok {
		if outcome, valid := value.(rejection); valid {
			// 日志正文已使用 message 键，拒绝说明单独命名，避免 JSON 出现重复键。
			fields = append(fields, zap.Int32("code", int32(outcome.response.Code)), zap.String("reason", outcome.response.Message))
			if len(outcome.response.Details) > 0 {
				fields = append(fields, zap.Any("details", outcome.response.Details))
			}
		} else {
			invalidState = true
		}
	}
	if value, ok := c.Get(summariesKey); ok {
		if summaries, valid := value.([]errorSummary); valid {
			fields = append(fields, zap.Int("error_count", len(c.Errors)), zap.Any("errors", summaries))
			if len(c.Errors) > 16 {
				fields = append(fields, zap.Bool("errors_truncated", true))
			}
		} else {
			invalidState = true
		}
	}
	// Context 键可能被其他中间件覆盖；只报告状态异常，不展开未知值。
	if invalidState {
		fields = append(fields, zap.Bool("diagnostic_state_invalid", true))
	}
	return level, fields
}
