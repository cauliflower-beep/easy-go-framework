/**
 * @Author: LiuShuXin
 * @Description: 真实 SDK 经本地 HTTP 服务验证双提供方对象操作与签名边界
 * @File: client_test.go
 * Software: Goland
 * @Date: 2026/10/15
 */
package objectstore

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cauliflower-beep/easy-go-framework/config"
	"github.com/cauliflower-beep/easy-go-framework/httpclient"
	"go.uber.org/zap"
)

func testOptions(provider string) Options {
	o := DefaultOptions()
	o.Provider = provider
	o.Endpoint = "http://storage.test"
	o.Region = "cn-hangzhou"
	o.Bucket = "framework-test"
	o.AccessKeyID = "test-id"
	o.AccessKeySecret = "test-secret"
	o.AllowInsecureHTTP = true
	o.UsePathStyle = provider == ProviderS3
	return o
}
func testClient(t *testing.T, provider string, handler http.HandlerFunc, configure func(*Options)) *Client {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	o := testOptions(provider)
	if configure != nil {
		configure(&o)
	}
	target, _ := url.Parse(s.URL)
	// 测试只改实际连接目标，保留已签名 Host。公网无需配置 DNS 或修改 hosts。
	route := func(next http.RoundTripper) http.RoundTripper {
		return httpclient.RoundTripperFunc(func(r *http.Request) (*http.Response, error) {
			clone := r.Clone(r.Context())
			clone.Host = r.URL.Host
			clone.URL.Host = target.Host
			return next.RoundTrip(clone)
		})
	}
	c, err := New(o, zap.NewNop(), route)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	return c
}

func crc(data []byte) string {
	return strconv.FormatUint(crc64.Checksum(data, crc64.MakeTable(crc64.ECMA)), 10)
}

func TestSDKObjectOperations(t *testing.T) {
	for _, provider := range []string{ProviderS3, ProviderOSS} {
		t.Run(provider, func(t *testing.T) {
			var mu sync.Mutex
			var data []byte
			exists := false
			key := "文件夹/a +%?#.txt"
			c := testClient(t, provider, func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.Header.Get("Authorization") == "" {
					t.Error("missing SDK signature")
				}
				if provider == ProviderOSS && r.Host != "framework-test.storage.test" {
					t.Errorf("OSS host %q", r.Host)
				}
				if r.URL.Path == "/framework-test" || r.URL.Path == "/" {
					if provider == ProviderOSS {
						fmt.Fprint(w, "<BucketInfo><Bucket><Name>framework-test</Name></Bucket></BucketInfo>")
					}
					return
				}
				actual := r.URL.Path
				if provider == ProviderS3 {
					actual = strings.TrimPrefix(actual, "/framework-test")
				}
				if actual != "/"+key {
					t.Errorf("key changed: %q", r.URL.Path)
				}
				switch r.Method {
				case "PUT":
					var err error
					data, err = io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
					}
					exists = true
					if r.Header.Get("Content-Type") != "text/plain" {
						t.Error("content type lost")
					}
					prefix := "X-Amz-Meta-"
					if provider == ProviderOSS {
						prefix = "X-Oss-Meta-"
					}
					if r.Header.Get(prefix+"source") != "unit-test" {
						t.Error("metadata lost")
					}
					w.Header().Set("ETag", `"opaque-tag"`)
					w.Header().Set("x-oss-hash-crc64ecma", crc(data))
				case "GET", "HEAD":
					if !exists {
						w.WriteHeader(404)
						fmt.Fprint(w, "<Error><Code>NoSuchKey</Code><Message>private-key</Message></Error>")
						return
					}
					w.Header().Set("Content-Length", strconv.Itoa(len(data)))
					w.Header().Set("Content-Type", "text/plain")
					w.Header().Set("ETag", `"opaque-tag"`)
					w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
					w.Header().Set("x-oss-hash-crc64ecma", crc(data))
					w.Header().Set("X-Amz-Meta-source", "unit-test")
					w.Header().Set("X-Oss-Meta-source", "unit-test")
					if r.Method == "GET" {
						_, _ = w.Write(data)
					}
				case "DELETE":
					exists = false
					w.WriteHeader(204)
				default:
					t.Errorf("unexpected method %s", r.Method)
				}
			}, nil)
			ctx := context.Background()
			if err := c.Ping(ctx); err != nil {
				t.Fatal(err)
			}
			body := strings.NewReader("skip:hello世界")
			_, _ = body.Seek(5, io.SeekStart)
			info, err := c.Put(ctx, PutInput{Key: key, Body: body, ContentType: "text/plain", Metadata: map[string]string{"source": "unit-test"}})
			if err != nil {
				t.Fatal(err)
			}
			if info.Size != int64(len("hello世界")) || info.ETag != `"opaque-tag"` {
				t.Fatalf("upload info %+v", info)
			}
			stat, err := c.Stat(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			if stat.Size != info.Size || stat.Metadata["source"] != "unit-test" || stat.LastModified.IsZero() {
				t.Fatalf("stat %+v", stat)
			}
			obj, err := c.Get(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(obj.Body)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != "hello世界" {
				t.Fatalf("content %q", got)
			}
			if err = obj.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if err = c.Delete(ctx, key); err != nil {
				t.Fatal(err)
			}
			if err = c.Delete(ctx, key); err != nil {
				t.Fatal(err)
			}
			_, err = c.Get(ctx, key)
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("not found %v", err)
			}
			if err = c.Shutdown(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPresignAndPublicEndpoint(t *testing.T) {
	for _, provider := range []string{ProviderS3, ProviderOSS} {
		t.Run(provider, func(t *testing.T) {
			o := testOptions(provider)
			o.PublicEndpoint = "https://public.example.com"
			o.SessionToken = "private-session-token"
			c, err := New(o, zap.NewNop())
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			for _, method := range []string{"GET", "PUT"} {
				var signed SignedRequest
				if method == "GET" {
					signed, err = c.PresignGet(context.Background(), "目录/has +%?#.txt", time.Minute)
				} else {
					signed, err = c.PresignPut(context.Background(), "目录/has +%?#.txt", time.Minute)
				}
				if err != nil {
					t.Fatal(err)
				}
				u, err := url.Parse(signed.URL)
				if err != nil {
					t.Fatal(err)
				}
				host := "public.example.com"
				if provider == ProviderOSS {
					host = "framework-test." + host
				}
				if u.Host != host || signed.Method != method || u.RawQuery == "" {
					t.Fatalf("invalid signing result %v %s", signed, u.Host)
				}
				if signed.ExpiresAt.Before(time.Now().Add(55*time.Second)) || signed.ExpiresAt.After(time.Now().Add(65*time.Second)) {
					t.Fatalf("expiration %v", signed.ExpiresAt)
				}
				if strings.Contains(fmt.Sprintf("%+v %#v", signed, signed), "private-session-token") {
					t.Error("signature formatting leaked")
				}
			}
		})
	}
}

func TestOptionsAndInputValidation(t *testing.T) {
	base := testOptions(ProviderS3)
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Options){
		"provider": func(o *Options) { o.Provider = "unknown" }, "bucket": func(o *Options) { o.Bucket = "a.b" }, "region": func(o *Options) { o.Region = "bad/region" }, "credentials": func(o *Options) { o.AccessKeySecret = "" },
		"endpoint": func(o *Options) { o.Endpoint = "https://user:pass@host" }, "missing endpoint": func(o *Options) { o.Endpoint = "" }, "HTTP opt-in": func(o *Options) { o.AllowInsecureHTTP = false }, "query": func(o *Options) { o.Endpoint = "https://host?secret" }, "path": func(o *Options) { o.Endpoint = "https://host/path" }, "public": func(o *Options) { o.PublicEndpoint = "bad" },
		"OSS path": func(o *Options) { o.Provider = ProviderOSS }, "timeout": func(o *Options) { o.OperationTimeout = 0 }, "redirect": func(o *Options) { o.HTTP.MaxRedirects = 1 }, "limit": func(o *Options) { o.HTTP.MaxResponseBytes = 1024 },
	} {
		t.Run(name, func(t *testing.T) {
			o := base
			mutate(&o)
			if o.Validate() == nil {
				t.Fatal("accepted invalid config")
			}
		})
	}
	jsonOptions, _ := json.Marshal(base)
	if strings.Contains(string(jsonOptions), "test-secret") || strings.Contains(fmt.Sprintf("%+v %#v", base, base), "test-secret") {
		t.Error("credentials leaked")
	}
	for _, key := range []string{"", "/leading", "\\leading", strings.Repeat("x", 1025), "bad\nkey", string([]byte{0xff})} {
		if validKey(key) == nil {
			t.Errorf("accepted key %q", key)
		}
	}
	if _, err := validatePut(PutInput{Key: "key"}); err == nil {
		t.Error("nil body accepted")
	}
	if _, err := validatePut(PutInput{Key: "key", Body: bytes.NewReader(nil), Metadata: map[string]string{"Invalid": "v"}}); err == nil {
		t.Error("invalid metadata accepted")
	}
	c, err := New(base, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, expiry := range []time.Duration{0, time.Millisecond, 7*24*time.Hour + time.Second} {
		if _, err = c.PresignGet(context.Background(), "key", expiry); err == nil {
			t.Error("invalid expiry accepted")
		}
	}
}

func TestNoSDKRetry(t *testing.T) {
	for _, provider := range []string{ProviderS3, ProviderOSS} {
		t.Run(provider, func(t *testing.T) {
			var attempts atomic.Int32
			c := testClient(t, provider, func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				w.WriteHeader(503)
				fmt.Fprint(w, "<Error><Code>ServiceUnavailable</Code><Message>secret</Message></Error>")
			}, nil)
			_, err := c.Put(context.Background(), PutInput{Key: "private-key", Body: strings.NewReader("private-data")})
			if !errors.Is(err, ErrUnavailable) || attempts.Load() != 1 {
				t.Fatalf("attempts=%d error=%v", attempts.Load(), err)
			}
		})
	}
}

func TestStoredGzipIsNotDecompressed(t *testing.T) {
	var buf bytes.Buffer
	z := gzip.NewWriter(&buf)
	_, _ = z.Write([]byte("stored compressed content"))
	_ = z.Close()
	data := buf.Bytes()
	for _, provider := range []string{ProviderS3, ProviderOSS} {
		t.Run(provider, func(t *testing.T) {
			c := testClient(t, provider, func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Accept-Encoding") != "identity" {
					t.Error("identity encoding required")
				}
				w.Header().Set("Content-Encoding", "gzip")
				w.Header().Set("Content-Length", strconv.Itoa(len(data)))
				_, _ = w.Write(data)
			}, nil)
			obj, err := c.Get(context.Background(), "file.gz")
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(obj.Body)
			_ = obj.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, data) {
				t.Fatal("stored bytes transparently changed")
			}
		})
	}
}

func TestConfigComposition(t *testing.T) {
	type settings struct {
		Store Options `mapstructure:"store"`
	}
	var base settings
	t.Setenv("STORE_FIXTURE_STORE_PROVIDER", "oss")
	t.Setenv("STORE_FIXTURE_STORE_ENDPOINT", "https://oss-cn-hangzhou.aliyuncs.com")
	t.Setenv("STORE_FIXTURE_STORE_REGION", "cn-hangzhou")
	t.Setenv("STORE_FIXTURE_STORE_BUCKET", "test-bucket")
	t.Setenv("STORE_FIXTURE_STORE_ACCESS_KEY_ID", "test-id")
	t.Setenv("STORE_FIXTURE_STORE_ACCESS_KEY_SECRET", "test-secret")
	if err := config.Load("", &base, config.Options{EnvPrefix: "STORE_FIXTURE", Defaults: map[string]any{"store.operation_timeout": time.Minute, "store.http.max_response_bytes": int64(-1)}}); err != nil {
		t.Fatal(err)
	}
	if base.Store.Provider != ProviderOSS || base.Store.AccessKeySecret != "test-secret" || base.Store.HTTP.MaxResponseBytes != -1 || base.Store.OperationTimeout != time.Minute {
		t.Fatal("configuration composition failed")
	}
}

func TestEmptyUploadAndKeyPreservation(t *testing.T) {
	for _, provider := range []string{ProviderS3, ProviderOSS} {
		t.Run(provider, func(t *testing.T) {
			key := "nested/a/../b//./final"
			c := testClient(t, provider, func(w http.ResponseWriter, r *http.Request) {
				path := r.URL.Path
				if provider == ProviderS3 {
					path = strings.TrimPrefix(path, "/framework-test")
				}
				if path != "/"+key {
					t.Errorf("object key normalized: %q", path)
				}
				data, err := io.ReadAll(r.Body)
				if err != nil || len(data) != 0 {
					t.Errorf("empty upload %d %v", len(data), err)
				}
				w.Header().Set("x-oss-hash-crc64ecma", crc(data))
			}, nil)
			if _, err := c.Put(context.Background(), PutInput{Key: key, Body: bytes.NewReader(nil)}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
