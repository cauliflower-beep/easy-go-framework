/**
 * @Author: LiuShuXin
 * @Description: Redis 单节点连接、TLS 和有界连接池配置
 * @File: options.go
 * Software: Goland
 * @Date: 2026/10/14
 */

// Package redis 提供单节点 Redis 客户端的配置、诊断与生命周期。
package redis

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
	"unicode"

	goredis "github.com/redis/go-redis/v9"
	"github.com/redis/go-redis/v9/maintnotifications"
)

// Options 不包含应用是否启用 Redis 的决策，也不读取进程环境变量。
// CommandTimeout 限制一次 WithClient 回调的总期限；Read/WriteTimeout 限制网络 I/O。
// WithClientTimeout 可以按次覆盖回调期限；阻塞命令的读取期限由驱动按命令设置。
// ShutdownTimeout 限制 Shutdown 排空与资源回收的总等待，也限制 Close 的收尾等待。
type Options struct {
	Host            string        `mapstructure:"host"`
	Port            int           `mapstructure:"port"`
	Database        int           `mapstructure:"database"`
	User            string        `mapstructure:"user"`
	Password        string        `mapstructure:"password" json:"-"`
	ClientName      string        `mapstructure:"client_name"`
	TLS             bool          `mapstructure:"tls"`
	TLSRootCert     string        `mapstructure:"tls_root_cert"`
	ConnectTimeout  time.Duration `mapstructure:"connect_timeout"`
	CommandTimeout  time.Duration `mapstructure:"command_timeout"`
	ReadTimeout     time.Duration `mapstructure:"read_timeout"`
	WriteTimeout    time.Duration `mapstructure:"write_timeout"`
	PoolTimeout     time.Duration `mapstructure:"pool_timeout"`
	HealthTimeout   time.Duration `mapstructure:"health_timeout"`
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout"`
	PoolSize        int           `mapstructure:"pool_size"`
	MinIdleConns    int           `mapstructure:"min_idle_conns"`
	ConnMaxIdleTime time.Duration `mapstructure:"conn_max_idle_time"`
	ConnMaxLifetime time.Duration `mapstructure:"conn_max_lifetime"`
	SlowThreshold   time.Duration `mapstructure:"slow_threshold"`
}

// DefaultOptions 是组件默认值；调用方可覆盖，但 Open 始终执行完整校验。
func DefaultOptions() Options {
	return Options{Host: "127.0.0.1", Port: 6379, TLS: true,
		ConnectTimeout: 5 * time.Second, CommandTimeout: 5 * time.Second,
		ReadTimeout: 3 * time.Second, WriteTimeout: 3 * time.Second, PoolTimeout: time.Second,
		HealthTimeout: 2 * time.Second, ShutdownTimeout: 5 * time.Second,
		PoolSize: 10, ConnMaxIdleTime: 5 * time.Minute, ConnMaxLifetime: 30 * time.Minute,
		SlowThreshold: 200 * time.Millisecond}
}

// Validate 合并所有配置问题，不在错误消息中包含原始配置值。
func (o Options) Validate() error {
	var problems []error
	if o.Host == "" || strings.TrimSpace(o.Host) != o.Host || strings.ContainsAny(o.Host, " /\\,=@?#") || strings.ContainsFunc(o.Host, unicode.IsControl) || (strings.Contains(o.Host, ":") && net.ParseIP(o.Host) == nil) {
		problems = append(problems, errors.New("redis.host must be a single IP address or hostname without port"))
	}
	if o.Port < 1 || o.Port > 65535 {
		problems = append(problems, errors.New("redis.port must be between 1 and 65535"))
	}
	if o.Database < 0 {
		problems = append(problems, errors.New("redis.database must not be negative"))
	}
	if strings.TrimSpace(o.User) != o.User || strings.ContainsFunc(o.User, unicode.IsControl) {
		problems = append(problems, errors.New("redis.user must not contain surrounding whitespace or control characters"))
	}
	if strings.ContainsRune(o.Password, '\x00') {
		problems = append(problems, errors.New("redis.password must not contain NUL"))
	}
	if len(o.ClientName) > 128 || strings.ContainsFunc(o.ClientName, unicode.IsSpace) || strings.ContainsFunc(o.ClientName, unicode.IsControl) {
		problems = append(problems, errors.New("redis.client_name must be at most 128 bytes without whitespace or control characters"))
	}
	if strings.ContainsFunc(o.TLSRootCert, unicode.IsControl) || (o.TLSRootCert != "" && !o.TLS) {
		problems = append(problems, errors.New("redis.tls_root_cert requires TLS and must not contain control characters"))
	}
	for _, field := range []struct {
		key   string
		value time.Duration
	}{
		{"connect_timeout", o.ConnectTimeout}, {"command_timeout", o.CommandTimeout},
		{"read_timeout", o.ReadTimeout}, {"write_timeout", o.WriteTimeout}, {"pool_timeout", o.PoolTimeout},
		{"health_timeout", o.HealthTimeout}, {"shutdown_timeout", o.ShutdownTimeout},
	} {
		if field.value <= 0 {
			problems = append(problems, fmt.Errorf("redis.%s must be positive", field.key))
		}
	}
	if o.PoolSize <= 0 {
		problems = append(problems, errors.New("redis.pool_size must be positive"))
	}
	if o.MinIdleConns < 0 || o.MinIdleConns > o.PoolSize {
		problems = append(problems, errors.New("redis.min_idle_conns must be between zero and pool_size"))
	}
	if o.ConnMaxIdleTime <= 0 || o.ConnMaxLifetime < 0 || o.SlowThreshold < 0 {
		problems = append(problems, errors.New("redis connection idle time must be positive; lifetime and slow threshold must not be negative"))
	}
	return errors.Join(problems...)
}

func (o Options) String() string {
	return fmt.Sprintf("redis.Options{Host:%q Port:%d Database:%d Password:[REDACTED] TLS:%t PoolSize:%d}", o.Host, o.Port, o.Database, o.TLS, o.PoolSize)
}
func (o Options) GoString() string { return o.String() }

// DriverOptions 为持有独立连接池的集成库创建已校验的驱动配置。
// 返回新配置，不暴露现有 Client；调用方负责所建连接的期限、诊断和关闭。
func (o Options) DriverOptions() (*goredis.Options, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	return clientOptions(o)
}

func clientOptions(o Options) (*goredis.Options, error) {
	settings := &goredis.Options{Network: "tcp", Addr: net.JoinHostPort(o.Host, fmt.Sprint(o.Port)),
		Username: o.User, Password: o.Password, DB: o.Database, ClientName: o.ClientName,
		DialTimeout: o.ConnectTimeout, ReadTimeout: o.ReadTimeout, WriteTimeout: o.WriteTimeout,
		PoolTimeout: o.PoolTimeout, PoolSize: o.PoolSize, MaxActiveConns: o.PoolSize, MinIdleConns: o.MinIdleConns,
		ConnMaxIdleTime: o.ConnMaxIdleTime, ConnMaxLifetime: o.ConnMaxLifetime,
		ContextTimeoutEnabled: true,
		// -1 才是禁用命令重试；0 在 go-redis 中会启用默认重试。
		MaxRetries: -1, DialerRetries: 1, PipelinePoolSize: -1,
		// 单节点通用组件采用 RESP2，关闭托管产品的自动维护协商和可选身份上报。
		Protocol: 2, DisableIdentity: true, MaintNotificationsConfig: &maintnotifications.Config{Mode: "disabled"},
	}
	if o.TLS {
		configuration := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: o.Host}
		if o.TLSRootCert != "" {
			pem, err := os.ReadFile(o.TLSRootCert)
			if err != nil {
				return nil, SafeError(err)
			}
			roots := x509.NewCertPool()
			if !roots.AppendCertsFromPEM(pem) {
				return nil, errors.New("redis CA certificate contains no valid certificate")
			}
			configuration.RootCAs = roots
		}
		settings.TLSConfig = configuration
	}
	return settings, nil
}
