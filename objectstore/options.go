/**
 * @Author: LiuShuXin
 * @Description: 对象存储提供方、连接与认证配置及跨提供方约束
 * @File: options.go
 * Software: Goland
 * @Date: 2026/10/15
 */

// Package objectstore 提供 S3/MinIO 与阿里 OSS 的公共对象操作。
package objectstore

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/cauliflower-beep/easy-go-framework/httpclient"
)

const (
	ProviderS3  = "s3"
	ProviderOSS = "oss"
)

// Options 由应用加载，不读取云 SDK 的环境变量或共享凭证文件。
// Endpoint 是不含 bucket 的服务地址；PublicEndpoint 仅用于签名，可为空。
type Options struct {
	Provider          string             `mapstructure:"provider"`
	Endpoint          string             `mapstructure:"endpoint"`
	PublicEndpoint    string             `mapstructure:"public_endpoint"`
	Region            string             `mapstructure:"region"`
	Bucket            string             `mapstructure:"bucket"`
	AccessKeyID       string             `mapstructure:"access_key_id" json:"-"`
	AccessKeySecret   string             `mapstructure:"access_key_secret" json:"-"`
	SessionToken      string             `mapstructure:"session_token" json:"-"`
	UsePathStyle      bool               `mapstructure:"use_path_style"`
	AllowInsecureHTTP bool               `mapstructure:"allow_insecure_http"`
	OperationTimeout  time.Duration      `mapstructure:"operation_timeout"`
	HTTP              httpclient.Options `mapstructure:"http"`
}

// DefaultOptions 为流式文件传输提供独立 HTTP 配置，不沿用普通 API 的 8 MiB 上限。
func DefaultOptions() Options {
	h := httpclient.DefaultOptions()
	h.RequestTimeout = 5 * time.Minute
	h.MaxResponseBytes = -1
	h.SlowThreshold = 0
	return Options{Provider: ProviderS3, Region: "us-east-1", OperationTimeout: 5 * time.Minute, HTTP: h}
}

var bucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)
var regionPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func (o Options) Validate() error {
	var errs []error
	if o.Provider != ProviderS3 && o.Provider != ProviderOSS {
		errs = append(errs, errors.New("objectstore.provider must be s3 or oss"))
	}
	if !bucketPattern.MatchString(o.Bucket) {
		errs = append(errs, errors.New("objectstore.bucket must be 3-63 lowercase letters, digits or hyphens with alphanumeric ends"))
	}
	if !regionPattern.MatchString(o.Region) {
		errs = append(errs, errors.New("objectstore.region is invalid"))
	}
	if strings.TrimSpace(o.AccessKeyID) == "" || strings.TrimSpace(o.AccessKeySecret) == "" {
		errs = append(errs, errors.New("objectstore credentials are required"))
	}
	for i, endpoint := range []string{o.Endpoint, o.PublicEndpoint} {
		if i == 1 && endpoint == "" {
			continue
		}
		if !validEndpoint(endpoint, o.AllowInsecureHTTP) {
			errs = append(errs, errors.New("objectstore endpoint must be an absolute HTTPS service URL without credentials, path, query or fragment; HTTP requires explicit opt-in"))
		}
	}
	if o.Provider == ProviderOSS && o.UsePathStyle {
		errs = append(errs, errors.New("objectstore OSS requires virtual hosted addressing"))
	}
	if o.OperationTimeout <= 0 {
		errs = append(errs, errors.New("objectstore.operation_timeout must be positive"))
	}
	if o.HTTP.MaxResponseBytes != -1 || o.HTTP.MaxRedirects != 0 {
		errs = append(errs, errors.New("objectstore HTTP must disable response size limits (-1) and redirects (0)"))
	}
	errs = append(errs, o.HTTP.Validate())
	return errors.Join(errs...)
}

func validEndpoint(value string, insecure bool) bool {
	u, err := url.Parse(value)
	return err == nil && (u.Scheme == "https" || insecure && u.Scheme == "http") && u.Host != "" && u.Hostname() != "" && u.User == nil && (u.Path == "" || u.Path == "/") && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && u.Opaque == "" && strings.TrimSpace(value) == value
}

// String/GoString 与 JSON 均不暴露凭证。
func (o Options) String() string {
	return fmt.Sprintf("objectstore.Options{Provider:%q Bucket:%q Region:%q Credentials:[REDACTED]}", o.Provider, o.Bucket, o.Region)
}
func (o Options) GoString() string { return o.String() }
