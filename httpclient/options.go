/**
 * @Author: LiuShuXin
 * @Description: 出站 HTTP 客户端的连接、期限、TLS 与响应限制配置
 * @File: options.go
 * Software: Goland
 * @Date: 2026/10/14
 */

// Package httpclient 提供通用出站 HTTP 请求、流式响应、安全诊断与生命周期。
package httpclient

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode"
)

// Options 不读取环境变量或包含外部服务的业务协议。
// RequestTimeout 包含连接等待、重定向及响应体读取；按次可用 RequestOptions 覆盖。
type Options struct {
	RequestTimeout         time.Duration `mapstructure:"request_timeout"`
	DialTimeout            time.Duration `mapstructure:"dial_timeout"`
	TLSHandshakeTimeout    time.Duration `mapstructure:"tls_handshake_timeout"`
	ResponseHeaderTimeout  time.Duration `mapstructure:"response_header_timeout"`
	ExpectContinueTimeout  time.Duration `mapstructure:"expect_continue_timeout"`
	IdleConnTimeout        time.Duration `mapstructure:"idle_conn_timeout"`
	ShutdownTimeout        time.Duration `mapstructure:"shutdown_timeout"`
	SlowThreshold          time.Duration `mapstructure:"slow_threshold"`
	MaxIdleConns           int           `mapstructure:"max_idle_conns"`
	MaxIdleConnsPerHost    int           `mapstructure:"max_idle_conns_per_host"`
	MaxConnsPerHost        int           `mapstructure:"max_conns_per_host"`
	MaxResponseHeaderBytes int64         `mapstructure:"max_response_header_bytes"`
	MaxResponseBytes       int64         `mapstructure:"max_response_bytes"`
	MaxRedirects           int           `mapstructure:"max_redirects"`
	HTTP2                  bool          `mapstructure:"http2"`
	TLSRootCert            string        `mapstructure:"tls_root_cert"`
	ProxyURL               string        `mapstructure:"proxy_url" json:"-"`
	UseEnvironmentProxy    bool          `mapstructure:"use_environment_proxy"`
}

// RequestOptions 的零值继承客户端配置；Timeout 必须非负。
// MaxResponseBytes 为 -1 时显式允许不限字节的流式响应，不改变请求期限。
type RequestOptions struct {
	Timeout          time.Duration
	MaxResponseBytes int64
}

func DefaultOptions() Options {
	return Options{
		RequestTimeout: 30 * time.Second, DialTimeout: 5 * time.Second,
		TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 10 * time.Second,
		ExpectContinueTimeout: time.Second, IdleConnTimeout: 90 * time.Second,
		ShutdownTimeout: 5 * time.Second, SlowThreshold: 200 * time.Millisecond,
		MaxIdleConns: 100, MaxIdleConnsPerHost: 10, MaxConnsPerHost: 20,
		MaxResponseHeaderBytes: 1 << 20, MaxResponseBytes: 8 << 20,
		HTTP2: true,
	}
}

// Validate 合并配置问题，不在错误中展开代理凭证或原始配置值。
func (o Options) Validate() error {
	var problems []error
	for _, field := range []struct {
		name  string
		value time.Duration
	}{
		{"request_timeout", o.RequestTimeout}, {"dial_timeout", o.DialTimeout},
		{"tls_handshake_timeout", o.TLSHandshakeTimeout}, {"response_header_timeout", o.ResponseHeaderTimeout},
		{"expect_continue_timeout", o.ExpectContinueTimeout}, {"idle_conn_timeout", o.IdleConnTimeout},
		{"shutdown_timeout", o.ShutdownTimeout},
	} {
		if field.value <= 0 {
			problems = append(problems, fmt.Errorf("httpclient.%s must be positive", field.name))
		}
	}
	if o.SlowThreshold < 0 {
		problems = append(problems, errors.New("httpclient.slow_threshold must not be negative"))
	}
	if o.MaxIdleConns <= 0 || o.MaxIdleConnsPerHost <= 0 || o.MaxConnsPerHost <= 0 || o.MaxIdleConnsPerHost > o.MaxIdleConns || o.MaxIdleConnsPerHost > o.MaxConnsPerHost {
		problems = append(problems, errors.New("httpclient connection limits must be positive; per-host idle limit must not exceed total idle or per-host connection limit"))
	}
	if o.MaxResponseHeaderBytes <= 0 || o.MaxResponseBytes == 0 || o.MaxResponseBytes < -1 {
		problems = append(problems, errors.New("httpclient response header limit must be positive; body limit must be positive or -1"))
	}
	if o.MaxRedirects < 0 {
		problems = append(problems, errors.New("httpclient.max_redirects must not be negative"))
	}
	if strings.ContainsFunc(o.TLSRootCert, unicode.IsControl) {
		problems = append(problems, errors.New("httpclient.tls_root_cert must not contain control characters"))
	}
	if o.ProxyURL != "" {
		proxy, err := url.Parse(o.ProxyURL)
		if err != nil || proxy == nil || (proxy.Scheme != "http" && proxy.Scheme != "https") || proxy.Host == "" || proxy.Opaque != "" || (proxy.Path != "" && proxy.Path != "/") || proxy.RawQuery != "" || proxy.Fragment != "" {
			problems = append(problems, errors.New("httpclient.proxy_url must be an HTTP(S) proxy address without path, query or fragment"))
		}
		if o.UseEnvironmentProxy {
			problems = append(problems, errors.New("httpclient proxy_url and use_environment_proxy must not both be set"))
		}
	}
	return errors.Join(problems...)
}

func (o Options) String() string {
	return fmt.Sprintf("httpclient.Options{RequestTimeout:%s MaxConnsPerHost:%d MaxResponseBytes:%d HTTP2:%t ProxyURL:[REDACTED]}", o.RequestTimeout, o.MaxConnsPerHost, o.MaxResponseBytes, o.HTTP2)
}
func (o Options) GoString() string { return o.String() }

func newTransport(o Options) (*http.Transport, error) {
	configuration := &tls.Config{MinVersion: tls.VersionTLS12}
	if o.TLSRootCert != "" {
		pem, err := os.ReadFile(o.TLSRootCert)
		if err != nil {
			return nil, SafeError(err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return nil, errors.New("HTTP CA certificate contains no valid certificate")
		}
		// 显式 CA 文件作为本客户端信任集，不修改进程全局证书或跳过验证。
		configuration.RootCAs = roots
	}
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(o.HTTP2)
	transport := &http.Transport{
		DialContext:     (&net.Dialer{Timeout: o.DialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig: configuration, TLSHandshakeTimeout: o.TLSHandshakeTimeout,
		ResponseHeaderTimeout: o.ResponseHeaderTimeout, ExpectContinueTimeout: o.ExpectContinueTimeout,
		IdleConnTimeout: o.IdleConnTimeout, MaxIdleConns: o.MaxIdleConns,
		MaxIdleConnsPerHost: o.MaxIdleConnsPerHost, MaxConnsPerHost: o.MaxConnsPerHost,
		MaxResponseHeaderBytes: o.MaxResponseHeaderBytes, Protocols: protocols,
	}
	if o.ProxyURL != "" {
		proxy, err := url.Parse(o.ProxyURL)
		if err != nil {
			return nil, SafeError(err)
		}
		transport.Proxy = http.ProxyURL(proxy)
	} else if o.UseEnvironmentProxy {
		transport.Proxy = http.ProxyFromEnvironment
	}
	return transport, nil
}
