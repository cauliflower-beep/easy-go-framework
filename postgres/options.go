/**
 * @Author: LiuShuXin
 * @Description: PostgreSQL 单主机连接与标准连接池配置校验
 * @File: options.go
 * Software: Goland
 * @Date: 2026/10/12
 */

// Package postgres 定义 PostgreSQL 基础组件的配置契约。
package postgres

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
	"unicode"
)

// Options 对应单个 TCP 目标；池参数沿用 database/sql 的语义。
// 创建连接池前统一校验，运行期不读取动态配置。
type Options struct {
	ApplicationName    string        `mapstructure:"application_name"`
	Host               string        `mapstructure:"host"`
	Port               int           `mapstructure:"port"`
	Database           string        `mapstructure:"database"`
	User               string        `mapstructure:"user"`
	Password           string        `mapstructure:"password" json:"-"`
	SSLMode            string        `mapstructure:"ssl_mode"`
	SSLRootCert        string        `mapstructure:"ssl_root_cert"`
	ConnectTimeout     time.Duration `mapstructure:"connect_timeout"`
	QueryTimeout       time.Duration `mapstructure:"query_timeout"`
	TransactionTimeout time.Duration `mapstructure:"transaction_timeout"`
	HealthTimeout      time.Duration `mapstructure:"health_timeout"`
	ShutdownTimeout    time.Duration `mapstructure:"shutdown_timeout"`
	SlowThreshold      time.Duration `mapstructure:"slow_threshold"`
	MaxOpenConns       int           `mapstructure:"max_open_conns"`
	MaxIdleConns       int           `mapstructure:"max_idle_conns"`
	ConnMaxLifetime    time.Duration `mapstructure:"conn_max_lifetime"`
	ConnMaxIdleTime    time.Duration `mapstructure:"conn_max_idle_time"`
}

// DefaultOptions 提供连接、池及期限默认值；应用必须补充 Database 和 User。
// 默认校验证书链与主机名，本地明文连接需显式选择 disable。
func DefaultOptions() Options {
	return Options{
		Host: "127.0.0.1", Port: 5432, SSLMode: "verify-full",
		ConnectTimeout: 5 * time.Second, QueryTimeout: 5 * time.Second,
		TransactionTimeout: 30 * time.Second, HealthTimeout: 2 * time.Second,
		ShutdownTimeout: 5 * time.Second, SlowThreshold: 200 * time.Millisecond,
		MaxOpenConns: 10, MaxIdleConns: 5,
		ConnMaxLifetime: 30 * time.Minute, ConnMaxIdleTime: 5 * time.Minute,
	}
}

// Validate 在连接前报告全部无效字段，错误不包含原始配置值。
func (o Options) Validate() error {
	var problems []error
	if o.Host == "" || strings.TrimSpace(o.Host) != o.Host || strings.ContainsAny(o.Host, " /\\,=") || strings.ContainsFunc(o.Host, unicode.IsControl) || (strings.Contains(o.Host, ":") && net.ParseIP(o.Host) == nil) {
		problems = append(problems, errors.New("postgres.host must be a single IP address or hostname without port"))
	}
	if o.Port < 1 || o.Port > 65535 {
		problems = append(problems, errors.New("postgres.port must be between 1 and 65535"))
	}
	for _, field := range []struct{ key, value string }{{"database", o.Database}, {"user", o.User}} {
		if strings.TrimSpace(field.value) == "" || strings.TrimSpace(field.value) != field.value || strings.ContainsFunc(field.value, unicode.IsControl) {
			problems = append(problems, fmt.Errorf("postgres.%s must be non-empty without surrounding whitespace or control characters", field.key))
		}
	}
	// 不裁剪密码；空密码是否可用由服务端认证方式决定。
	if strings.ContainsRune(o.Password, '\x00') {
		problems = append(problems, errors.New("postgres.password must not contain NUL"))
	}
	switch o.SSLMode {
	case "disable", "require", "verify-ca", "verify-full":
	default:
		// 不提供允许回退明文的模式，开发环境必须显式选择 disable。
		problems = append(problems, errors.New("postgres.ssl_mode must be disable, require, verify-ca, or verify-full"))
	}
	if strings.ContainsFunc(o.SSLRootCert, unicode.IsControl) {
		problems = append(problems, errors.New("postgres.ssl_root_cert must not contain control characters"))
	}
	if o.SSLRootCert != "" && o.SSLMode == "disable" {
		problems = append(problems, errors.New("postgres.ssl_root_cert requires TLS to be enabled"))
	}
	if len(o.ApplicationName) > 63 || strings.ContainsFunc(o.ApplicationName, unicode.IsControl) {
		problems = append(problems, errors.New("postgres.application_name must be at most 63 bytes without control characters"))
	}
	if o.SSLRootCert == "system" && o.SSLMode != "verify-full" {
		problems = append(problems, errors.New("postgres.ssl_root_cert=system requires verify-full"))
	}
	if o.QueryTimeout > 2147483647*time.Millisecond || o.TransactionTimeout > 2147483647*time.Millisecond {
		problems = append(problems, errors.New("postgres query and transaction timeouts exceed PostgreSQL limits"))
	}
	if o.ConnectTimeout <= 0 {
		problems = append(problems, errors.New("postgres.connect_timeout must be positive"))
	}
	for _, field := range []struct {
		key   string
		value time.Duration
	}{
		{"query_timeout", o.QueryTimeout}, {"transaction_timeout", o.TransactionTimeout},
		{"health_timeout", o.HealthTimeout}, {"shutdown_timeout", o.ShutdownTimeout},
	} {
		if field.value <= 0 {
			problems = append(problems, fmt.Errorf("postgres.%s must be positive", field.key))
		}
	}
	if o.SlowThreshold < 0 {
		problems = append(problems, errors.New("postgres.slow_threshold must not be negative"))
	}
	if o.MaxOpenConns <= 0 {
		problems = append(problems, errors.New("postgres.max_open_conns must be positive"))
	}
	if o.MaxIdleConns < 0 || o.MaxIdleConns > o.MaxOpenConns {
		problems = append(problems, errors.New("postgres.max_idle_conns must be between zero and max_open_conns"))
	}
	// 0 表示不按该时间淘汰连接；负数容易误配，提前拒绝。
	if o.ConnMaxLifetime < 0 {
		problems = append(problems, errors.New("postgres.conn_max_lifetime must not be negative"))
	}
	if o.ConnMaxIdleTime < 0 {
		problems = append(problems, errors.New("postgres.conn_max_idle_time must not be negative"))
	}
	return errors.Join(problems...)
}

// String 仅提供脱敏诊断；日志仍应显式选取必要字段。
func (o Options) String() string {
	return fmt.Sprintf("postgres.Options{Host:%q Port:%d Database:%q User:%q Password:[REDACTED] SSLMode:%q SSLRootCert:%q ConnectTimeout:%s MaxOpenConns:%d MaxIdleConns:%d ConnMaxLifetime:%s ConnMaxIdleTime:%s}",
		o.Host, o.Port, o.Database, o.User, o.SSLMode, o.SSLRootCert, o.ConnectTimeout, o.MaxOpenConns, o.MaxIdleConns, o.ConnMaxLifetime, o.ConnMaxIdleTime)
}

// GoString 同时覆盖 %#v，避免调试格式绕过脱敏。
func (o Options) GoString() string { return o.String() }
