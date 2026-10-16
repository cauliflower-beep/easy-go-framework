/**
 * @Author: LiuShuXin
 * @Description: 将应用错误集中映射为 HTTP 响应并记录安全异常诊断
 * @File: errors.go
 * Software: Goland
 * @Date: 2026/10/13
 */
package httpapi

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/cauliflower-beep/easy-go-framework/apperror"
	"github.com/cauliflower-beep/easy-go-framework/logging"
)

// Handler 的返回错误统一交给中间件；业务调用只传标准 Context 和普通参数。
type Handler func(*gin.Context) error

func Handle(handler Handler) gin.HandlerFunc {
	if handler == nil {
		panic("HTTP handler must not be nil")
	}
	return func(c *gin.Context) {
		if err := handler(c); err != nil {
			_ = c.Error(err)
			c.Abort()
		}
	}
}

// ErrorHandler 必须安装在路由之前、访问日志之后；不要再装 Gin 默认错误日志器。
func ErrorHandler(logger *zap.Logger) gin.HandlerFunc {
	if logger == nil {
		panic("HTTP error logger must not be nil")
	}
	return func(c *gin.Context) {
		c.Next()
		if len(c.Errors) == 0 {
			return
		}
		err := c.Errors.Last().Err
		status, response := errorResponse(err)
		response.RequestID = logging.RequestID(c.Request.Context())
		log := logging.ForRequest(logger, c.Request.Context())
		// 最后一个错误决定响应；所有登记事件保留顺序和诊断，不让后置 4xx 掩盖前置 5xx。
		summaries := make([]errorSummary, 0, min(len(c.Errors), 16))
		firstServerFailure := -1
		for index, item := range c.Errors {
			mapped, _ := errorResponse(item.Err)
			if mapped >= 500 {
				firstServerFailure = index
				break
			}
		}
		indices := diagnosticIndices(len(c.Errors), firstServerFailure)
		for _, index := range indices {
			mapped, public := errorResponse(c.Errors[index].Err)
			summaries = append(summaries, errorSummary{Index: index, Status: mapped, Code: public.Code, Message: public.Message, Details: public.Details})
		}
		c.Set(summariesKey, summaries)
		fields := []zap.Field{zap.Int("mapped_status", status), zap.Int32("code", int32(response.Code)), zap.String("error_type", fmt.Sprintf("%T", err)), zap.Int("error_count", len(c.Errors)), zap.String("route", c.FullPath()), zap.Bool("response_committed", c.Writer.Written())}
		if c.Writer.Written() {
			log.Error("HTTP error after response committed", append(fields, diagnosticFields(c, indices)...)...)
			c.Abort()
			return
		}
		recordOutcome(c, response)
		if firstServerFailure >= 0 {
			fields = append(fields, diagnosticFields(c, indices)...)
			log.Error("HTTP request failed", append(fields, zap.Stack("stack"))...)
		}
		c.Abort()
		if err := writeJSON(c, status, response); err != nil {
			diagnostics, truncated := logging.Diagnostics(err)
			log.Warn("HTTP error response write failed", zap.Any("causes", diagnostics), zap.Bool("causes_truncated", truncated))
		}
	}
}

func errorResponse(err error) (int, ErrorResponse) {
	status, code, message := http.StatusInternalServerError, apperror.CodeInternalError, "internal server error"
	var public *apperror.Error
	if errors.As(err, &public) && public != nil {
		knownKind := true
		switch public.Kind() {
		case apperror.Internal:
			// 保留公开故障码；内部消息仍由通用文案替代。
		case apperror.InvalidArgument:
			status, code, message = http.StatusBadRequest, apperror.CodeInvalidArgument, "invalid request parameters"
		case apperror.Unauthenticated:
			status, code, message = http.StatusUnauthorized, apperror.CodeUnauthenticated, "authentication required"
		case apperror.Forbidden:
			status, code, message = http.StatusForbidden, apperror.CodeForbidden, "access denied"
		case apperror.NotFound:
			status, code, message = http.StatusNotFound, apperror.CodeNotFound, "resource not found"
		case apperror.Conflict:
			status, code, message = http.StatusConflict, apperror.CodeConflict, "resource conflict"
		case apperror.Unavailable:
			status, code, message = http.StatusServiceUnavailable, apperror.CodeUnavailable, "service temporarily unavailable"
		default:
			knownKind = false
		}
		// 未知分类使用完整的通用兜底，避免错误定义绕过映射约束。
		if knownKind && public.Code() > 0 {
			code = public.Code()
		}
		if status != http.StatusInternalServerError {
			if public.Message() != "" {
				message = public.Message()
			}
		}
	}
	response := ErrorResponse{Code: code, Message: message}
	var validation *validationError
	if status == http.StatusBadRequest && errors.As(err, &validation) {
		response.Details = validation.details
	}
	var media *mediaTypeError
	if public == nil && errors.As(err, &media) {
		status, response.Code, response.Message = http.StatusUnsupportedMediaType, apperror.CodeUnsupportedMediaType, "application/json with identity encoding is required"
	}
	return status, response
}

type errorDiagnostic struct {
	Index     int                  `json:"index"`
	Causes    []logging.Diagnostic `json:"causes"`
	Truncated bool                 `json:"truncated,omitempty"`
}

func diagnosticFields(c *gin.Context, indices []int) []zap.Field {
	items := make([]errorDiagnostic, 0, min(len(c.Errors), 16))
	for _, index := range indices {
		causes, truncated := logging.Diagnostics(c.Errors[index].Err)
		items = append(items, errorDiagnostic{Index: index, Causes: causes, Truncated: truncated})
	}
	return []zap.Field{zap.Any("errors", items), zap.Bool("errors_truncated", len(c.Errors) > 16)}
}

// 超限时保留早期事件、首个内部失败和决定响应的最后事件，维持原登记顺序。
func diagnosticIndices(total, firstServer int) []int {
	count := min(total, 16)
	indices := make([]int, count)
	for index := range count {
		indices[index] = index
	}
	if total > 16 {
		indices[15] = total - 1
		if firstServer >= 15 && firstServer < total-1 {
			indices[14] = firstServer
		}
	}
	return indices
}
