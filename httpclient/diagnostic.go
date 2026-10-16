/**
 * @Author: LiuShuXin
 * @Description: 出站 HTTP 请求安全日志与响应消费诊断
 * @File: diagnostic.go
 * Software: Goland
 * @Date: 2026/10/14
 */
package httpclient

import (
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cauliflower-beep/easy-go-framework/logging"
	"go.uber.org/zap"
)

func safeHost(address *url.URL) string {
	host := address.Hostname()
	if len(host) == 0 || len(host) > 253 {
		return "[invalid host]"
	}
	if net.ParseIP(host) == nil {
		for _, char := range host {
			if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '.' || char == '-' || char == '_') {
				return "[nonstandard host]"
			}
		}
	}
	host = strings.ToLower(host)
	if port := address.Port(); port != "" {
		value, err := strconv.ParseUint(port, 10, 16)
		if err != nil || value == 0 {
			return "[invalid host]"
		}
		return net.JoinHostPort(host, strconv.FormatUint(value, 10))
	}
	return host
}

func safeMethod(method string) string {
	switch method {
	case "", http.MethodGet:
		return http.MethodGet
	case http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete,
		http.MethodConnect, http.MethodOptions, http.MethodTrace, http.MethodPatch:
		return method
	default:
		return "OTHER"
	}
}

func (op *operation) logOutcome(err error, complete bool, bytes int64) {
	elapsed := time.Since(op.started)
	failed := err != nil || op.status >= 400
	slow := op.client.options.SlowThreshold > 0 && elapsed >= op.client.options.SlowThreshold
	if !failed && !slow {
		return
	}
	fields := []zap.Field{
		zap.String("method", op.method), zap.String("scheme", op.scheme), zap.String("host", op.host), zap.Int("status", op.status),
		zap.Duration("duration", elapsed), zap.Duration("header_duration", op.headerDuration),
		zap.Int64("response_bytes", bytes), zap.Bool("body_complete", complete),
	}
	// URL 路径、查询串、片段、用户信息、头和正文都不进入日志。
	log := logging.ForRequest(op.client.logger, op.ctx)
	if err != nil {
		log.Warn("HTTP client request failed", append(fields, logging.ErrorFields(SafeError(err))...)...)
	} else if op.status >= 400 {
		log.Warn("HTTP client error response", fields...)
	} else {
		log.Warn("HTTP client request slow", fields...)
	}
}
