/**
 * @Author: LiuShuXin
 * @Description: 请求关联、访问日志、异常恢复及有界请求体读取
 * @File: middleware.go
 * Software: Goland
 * @Date: 2026/10/10 11:10
 */

package httpserver

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/cauliflower-beep/easy-go-framework/apperror"
	"github.com/cauliflower-beep/easy-go-framework/httpapi"
	"github.com/cauliflower-beep/easy-go-framework/logging"
)

const requestIDHeader = "X-Request-ID"

func requestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 入口生成标识，不直接信任外部客户端传入的关联字段。
		id := newRequestID()
		c.Request = c.Request.WithContext(logging.WithRequestID(c.Request.Context(), id))
		c.Header(requestIDHeader, id)
		c.Next()
	}
}

func accessLog(logger *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		log := logging.ForRequest(logger, c.Request.Context())
		// 不记录查询串、请求头或请求体，避免凭证和业务数据进入日志。
		fields := []zap.Field{zap.String("method", c.Request.Method), zap.String("path", c.Request.URL.Path), zap.String("ip", c.ClientIP())}
		log.Debug("HTTP request started", fields...)
		defer func() {
			fields = append(fields, zap.Int("status", c.Writer.Status()), zap.Duration("duration", time.Since(start)), zap.Int("response_bytes", c.Writer.Size()))
			level, outcomeFields := httpapi.Completion(c)
			fields = append(fields, outcomeFields...)
			if event := log.Check(level, "HTTP request completed"); event != nil {
				event.Write(fields...)
			}
		}()
		c.Next()
	}
}

func recovery(logger *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if value := recover(); value != nil {
				logging.ForRequest(logger, c.Request.Context()).Error("HTTP panic recovered",
					zap.String("reason", "handler panicked"), zap.ByteString("panic_stack", debug.Stack()), zap.Bool("response_committed", c.Writer.Written()))
				// 已发送的状态码无法改写；不追加错误 JSON 破坏原响应体。
				if !c.Writer.Written() {
					respondError(c, http.StatusInternalServerError, apperror.CodeInternalError, "internal server error")
				}
				c.Abort()
			}
		}()
		c.Next()
	}
}

func bodyLimit(limit int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.ContentLength > limit {
			closeRejectedBody(c)
			respondError(c, http.StatusRequestEntityTooLarge, apperror.CodeBodyTooLarge, "request body exceeds limit")
			return
		}
		if c.Request.Body == nil || c.Request.Body == http.NoBody {
			c.Next()
			return
		}
		// 当前 API 请求体有界读入内存；即使路由不读正文，也检查分块请求。
		// 后续大文件流式上传应采用独立路由策略，不扩大通用 API 限额。
		reader := http.MaxBytesReader(c.Writer, c.Request.Body, limit)
		data, err := io.ReadAll(reader)
		if err != nil {
			closeRejectedBody(c)
			var tooLarge *http.MaxBytesError
			var networkError net.Error
			switch {
			case errors.As(err, &tooLarge):
				respondError(c, http.StatusRequestEntityTooLarge, apperror.CodeBodyTooLarge, "request body exceeds limit")
			case errors.As(err, &networkError) && networkError.Timeout():
				respondError(c, http.StatusRequestTimeout, apperror.CodeRequestTimeout, "request body read timed out")
			default:
				respondError(c, http.StatusBadRequest, apperror.CodeInvalidBody, "cannot read request body")
			}
			return
		}
		_ = reader.Close()
		c.Request.Body = io.NopCloser(bytes.NewReader(data))
		c.Next()
	}
}

func closeRejectedBody(c *gin.Context) {
	if c.Request.ProtoMajor == 1 {
		// HTTP/1 复用连接前会尝试读完正文；拒绝后关闭连接并终止读取，
		// 避免分块正文未结束时，413 响应被剩余正文的读取拖到超时。
		c.Header("Connection", "close")
		_ = http.NewResponseController(c.Writer).SetReadDeadline(time.Now())
	}
	if c.Request.Body != nil {
		_ = c.Request.Body.Close()
	}
}

func respondError(c *gin.Context, status int, code apperror.Code, message string) {
	httpapi.Abort(c, status, code, message)
}

// CORS 组件只操作响应头；延后提交状态，让来源拒绝也使用统一 JSON。
// 允许来源及预检规则仍由 gin-contrib/cors 判断，不复制其匹配逻辑。
func corsResponses(handler gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		writer := c.Writer
		defer func() { c.Writer = writer }()
		c.Writer = &corsHeaders{ResponseWriter: writer}
		handler(c)
		c.Writer = writer
		if c.IsAborted() {
			if writer.Status() == http.StatusForbidden {
				respondError(c, http.StatusForbidden, apperror.CodeCORSForbidden, "request origin is not allowed")
			} else {
				writer.WriteHeaderNow()
			}
		}
	}
}

type corsHeaders struct{ gin.ResponseWriter }

func (*corsHeaders) WriteHeaderNow() {}
