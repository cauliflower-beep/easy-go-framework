/**
 * @Author: LiuShuXin
 * @Description: HTTP 监听、超时、报文限制、代理与跨域配置及校验
 * @File: options.go
 * Software: Goland
 * @Date: 2026/10/10 11:10
 */

package httpserver

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// Options 使用明确的时间单位和字节数；Port 为 0 时由系统分配端口。
type Options struct {
	Host              string        `mapstructure:"host"`
	Port              int           `mapstructure:"port"`
	ReadHeaderTimeout time.Duration `mapstructure:"read_header_timeout"`
	ReadTimeout       time.Duration `mapstructure:"read_timeout"`
	WriteTimeout      time.Duration `mapstructure:"write_timeout"`
	IdleTimeout       time.Duration `mapstructure:"idle_timeout"`
	ShutdownTimeout   time.Duration `mapstructure:"shutdown_timeout"`
	MaxHeaderBytes    int           `mapstructure:"max_header_bytes"`
	BodyLimit         int64         `mapstructure:"body_limit"`
	TrustedProxies    []string      `mapstructure:"trusted_proxies"`
	CORS              CORS          `mapstructure:"cors"`
}

// CORS 未配置来源时不开放跨域；浏览器可读取 X-Request-ID 响应头。
type CORS struct {
	AllowedOrigins   []string      `mapstructure:"allowed_origins"`
	AllowedMethods   []string      `mapstructure:"allowed_methods"`
	AllowedHeaders   []string      `mapstructure:"allowed_headers"`
	AllowCredentials bool          `mapstructure:"allow_credentials"`
	MaxAge           time.Duration `mapstructure:"max_age"`
}

// DefaultOptions 默认仅监听回环地址，面向有界 API 请求；应用显式开放监听地址。
func DefaultOptions() Options {
	return Options{
		Host: "127.0.0.1", Port: 8080,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second,
		ShutdownTimeout: 5 * time.Second, MaxHeaderBytes: 1 << 20, BodyLimit: 1 << 20,
		CORS: CORS{AllowedMethods: []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
			AllowedHeaders: []string{"Content-Type", "Authorization"}, MaxAge: 12 * time.Hour},
	}
}

// Validate 在监听前拒绝无效配置，避免静默禁用限制或接受错误代理。
func (h Options) Validate() error {
	var problems []error
	if h.Host == "" || strings.TrimSpace(h.Host) != h.Host || strings.ContainsAny(h.Host, " /\\\t\r\n") || (strings.Contains(h.Host, ":") && net.ParseIP(h.Host) == nil) {
		problems = append(problems, errors.New("http.host must be an IP address or hostname without port"))
	}
	if h.Port < 0 || h.Port > 65535 {
		problems = append(problems, errors.New("http.port must be between 0 and 65535"))
	}
	for name, value := range map[string]time.Duration{"read_header_timeout": h.ReadHeaderTimeout, "read_timeout": h.ReadTimeout, "write_timeout": h.WriteTimeout, "idle_timeout": h.IdleTimeout, "shutdown_timeout": h.ShutdownTimeout} {
		if value <= 0 {
			problems = append(problems, fmt.Errorf("http.%s must be positive", name))
		}
	}
	if h.ReadHeaderTimeout > h.ReadTimeout {
		problems = append(problems, errors.New("http.read_header_timeout must not exceed read_timeout"))
	}
	if h.MaxHeaderBytes <= 0 {
		problems = append(problems, errors.New("http.max_header_bytes must be positive"))
	}
	if h.BodyLimit <= 0 {
		problems = append(problems, errors.New("http.body_limit must be positive"))
	}
	for _, proxy := range h.TrustedProxies {
		if net.ParseIP(proxy) == nil {
			if _, _, err := net.ParseCIDR(proxy); err != nil {
				problems = append(problems, fmt.Errorf("http.trusted_proxies contains invalid IP/CIDR %q", proxy))
			}
		}
	}
	return errors.Join(append(problems, h.CORS.Validate())...)
}

// Validate 仅允许明确 HTTP 来源；凭证跨域必须指定具体来源。
func (c CORS) Validate() error {
	var problems []error
	for _, origin := range c.AllowedOrigins {
		if origin == "*" {
			if c.AllowCredentials {
				problems = append(problems, errors.New("http.cors.allow_credentials requires explicit origins"))
			}
			continue
		}
		u, err := url.Parse(origin)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || strings.Contains(origin, "*") {
			problems = append(problems, fmt.Errorf("http.cors.allowed_origins contains invalid origin %q", origin))
		}
	}
	if len(c.AllowedMethods) == 0 {
		problems = append(problems, errors.New("http.cors.allowed_methods must not be empty"))
	}
	for _, method := range c.AllowedMethods {
		if !httpToken(method) || method != strings.ToUpper(method) {
			problems = append(problems, fmt.Errorf("http.cors.allowed_methods contains invalid method %q", method))
		}
	}
	for _, header := range c.AllowedHeaders {
		if !httpToken(header) {
			problems = append(problems, fmt.Errorf("http.cors.allowed_headers contains invalid header %q", header))
		}
	}
	if c.MaxAge <= 0 {
		problems = append(problems, errors.New("http.cors.max_age must be positive"))
	}
	return errors.Join(problems...)
}

func httpToken(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", r)) {
			return false
		}
	}
	return true
}
