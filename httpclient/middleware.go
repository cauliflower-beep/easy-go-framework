/**
 * @Author: LiuShuXin
 * @Description: 构造时组装标准 RoundTripper 装饰链，保留客户端资源所有权
 * @File: middleware.go
 * Software: Goland
 * @Date: 2026/10/15
 */
package httpclient

import (
	"fmt"
	"net/http"
	"reflect"
)

// Middleware 装饰 next，多个参数按 A -> B -> Transport 顺序处理请求、反向返回响应。
// 每个客户端构造时各调用一次；返回值须支持并发，并保留 Context、请求体和响应体语义。
// 注入请求头应先 Clone 请求；不得改写 next 的连接配置或绕过它另建连接池。
// 独立采集器/导出器等资源由调用方管理，客户端只关闭自身拥有的 Transport。
type Middleware func(next http.RoundTripper) http.RoundTripper

// RoundTripperFunc 将函数适配成标准 RoundTripper，便于编写轻量装饰器。
type RoundTripperFunc func(*http.Request) (*http.Response, error)

func (f RoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func chainTransport(base http.RoundTripper, middlewares []Middleware) (http.RoundTripper, error) {
	// 先检查全部参数，避免无效列表已经调用部分构造函数。
	for i, middleware := range middlewares {
		if middleware == nil {
			return nil, fmt.Errorf("httpclient middleware[%d] must not be nil", i)
		}
	}
	next := base
	for i := len(middlewares) - 1; i >= 0; i-- {
		next = middlewares[i](next)
		if nilRoundTripper(next) {
			return nil, fmt.Errorf("httpclient middleware[%d] must return a non-nil RoundTripper", i)
		}
	}
	return next, nil
}

func nilRoundTripper(transport http.RoundTripper) bool {
	if transport == nil {
		return true
	}
	// 包括带 nil 指针/函数的接口值，防止构造成功后在首次请求中才发生 panic。
	value := reflect.ValueOf(transport)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
