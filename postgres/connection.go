/**
 * @Author: LiuShuXin
 * @Description: 用明确字段构造单主机 pgx 配置，隔离 PG 环境变量与隐式 TLS 回退
 * @File: connection.go
 * Software: Goland
 * @Date: 2026/10/12
 */
package postgres

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

func connectionConfig(o Options) (*pgx.ConnConfig, error) {
	// pgx 不能通过空 service 值禁用服务文件；显式拒绝进程级服务配置。
	// 不修改全局环境变量，避免影响同进程的其他组件或并发初始化。
	if os.Getenv("PGSERVICE") != "" {
		return nil, ErrAmbientService
	}
	// 固定无密钥模板取得 pgx 内部标记，再逐项赋值；不拼接密码到 DSN。
	c, err := pgx.ParseConfig("host=localhost port=5432 user=service dbname=service password='' sslmode=disable passfile='' sslcert='' sslkey='' sslrootcert='' connect_timeout=5 target_session_attrs=any sslnegotiation=postgres channel_binding=prefer require_auth='' min_protocol_version=3.0 max_protocol_version=3.0")
	if err != nil {
		return nil, err
	}
	c.Host = o.Host
	c.Port = uint16(o.Port)
	c.Database = o.Database
	c.User = o.User
	c.Password = o.Password
	c.ValidateConnect = nil
	c.SSLNegotiation = "postgres"
	c.ChannelBinding = "prefer"
	c.ConnectTimeout = o.ConnectTimeout
	c.Fallbacks = nil
	name := o.ApplicationName
	if name == "" {
		name = "easy-framework"
	}
	c.RuntimeParams = map[string]string{"application_name": name, "timezone": "UTC",
		"statement_timeout":                   strconv.FormatInt((int64(o.QueryTimeout)+int64(time.Millisecond)-1)/int64(time.Millisecond), 10),
		"idle_in_transaction_session_timeout": strconv.FormatInt((int64(o.TransactionTimeout)+int64(time.Millisecond)-1)/int64(time.Millisecond), 10),
	}
	if o.SSLMode == "disable" {
		c.TLSConfig = nil
		return c, nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: o.Host}
	verifyCA := o.SSLMode == "verify-ca" || (o.SSLMode == "require" && o.SSLRootCert != "")
	var roots *x509.CertPool
	if o.SSLMode == "verify-full" || verifyCA {
		var err error
		roots, err = x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
		if o.SSLRootCert != "" && o.SSLRootCert != "system" {
			pem, err := os.ReadFile(o.SSLRootCert)
			if err != nil {
				return nil, err
			}
			roots = x509.NewCertPool()
			if !roots.AppendCertsFromPEM(pem) {
				return nil, errors.New("invalid PostgreSQL CA certificate")
			}
		}
		cfg.RootCAs = roots
	}
	if o.SSLMode == "require" || verifyCA {
		// require 只要求加密；verify-ca 单独校验证书链，不校验主机名。
		cfg.InsecureSkipVerify = true
		if verifyCA {
			cfg.VerifyConnection = func(state tls.ConnectionState) error {
				if len(state.PeerCertificates) == 0 {
					return errors.New("missing PostgreSQL peer certificate")
				}
				intermediates := x509.NewCertPool()
				for _, cert := range state.PeerCertificates[1:] {
					intermediates.AddCert(cert)
				}
				_, err := state.PeerCertificates[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates})
				return err
			}
		}
	}
	c.TLSConfig = cfg
	return c, nil
}
