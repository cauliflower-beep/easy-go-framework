/**
 * @Author: LiuShuXin
 * @Description: 专用 bucket 的真实对象服务集成测试，仅清理本次随机对象
 * @File: integration_test.go
 * Software: Goland
 * @Date: 2026/10/15
 */
package objectstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestIntegrationObjectRoundTrip(t *testing.T) {
	endpoint := os.Getenv("OBJECTSTORE_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("OBJECTSTORE_TEST_ENDPOINT not set; real service not tested")
	}
	o := DefaultOptions()
	o.Endpoint = endpoint
	if provider := os.Getenv("OBJECTSTORE_TEST_PROVIDER"); provider != "" {
		o.Provider = provider
	}
	if region := os.Getenv("OBJECTSTORE_TEST_REGION"); region != "" {
		o.Region = region
	}
	o.Bucket = os.Getenv("OBJECTSTORE_TEST_BUCKET")
	o.AccessKeyID = os.Getenv("OBJECTSTORE_TEST_ACCESS_KEY_ID")
	o.AccessKeySecret = os.Getenv("OBJECTSTORE_TEST_ACCESS_KEY_SECRET")
	o.SessionToken = os.Getenv("OBJECTSTORE_TEST_SESSION_TOKEN")
	o.PublicEndpoint = os.Getenv("OBJECTSTORE_TEST_PUBLIC_ENDPOINT")
	o.HTTP.TLSRootCert = os.Getenv("OBJECTSTORE_TEST_TLS_ROOT_CERT")
	o.UsePathStyle = o.Provider == ProviderS3
	for name, target := range map[string]*bool{"OBJECTSTORE_TEST_PATH_STYLE": &o.UsePathStyle, "OBJECTSTORE_TEST_ALLOW_HTTP": &o.AllowInsecureHTTP} {
		if value := os.Getenv(name); value != "" {
			parsed, err := strconv.ParseBool(value)
			if err != nil {
				t.Fatalf("invalid boolean %s", name)
			}
			*target = parsed
		}
	}
	c, err := New(o, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err = c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	var random [16]byte
	if _, err = rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	prefix := "framework-test/" + hex.EncodeToString(random[:])
	key := prefix + "/对象 +%.txt"
	signedKey := prefix + "/signed.txt"
	// 在发起写入前注册清理，涵盖“客户端超时但服务端已写入”的情况。
	t.Cleanup(func() {
		clean, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		for _, key := range []string{key, signedKey} {
			if err := c.Delete(clean, key); err != nil {
				t.Error(err)
			}
		}
	})
	payload := "framework integration test payload"
	_, err = c.Put(ctx, PutInput{Key: key, Body: strings.NewReader(payload), ContentType: "text/plain", Metadata: map[string]string{"source": "framework-test"}})
	if err != nil {
		t.Fatal(err)
	}
	info, err := c.Stat(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != int64(len(payload)) || info.Metadata["source"] != "framework-test" {
		t.Fatal("metadata mismatch")
	}
	obj, err := c.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(obj.Body)
	_ = obj.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != payload {
		t.Fatal("download differs")
	}
	for _, method := range []string{"PUT", "GET"} {
		var signed SignedRequest
		var body io.Reader
		if method == "PUT" {
			signed, err = c.PresignPut(ctx, signedKey, time.Minute)
			body = strings.NewReader(payload)
		} else {
			signed, err = c.PresignGet(ctx, signedKey, time.Minute)
		}
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequestWithContext(ctx, method, signed.URL, body)
		if err != nil {
			t.Fatal("invalid signed request")
		}
		req.Header = signed.Headers.Clone()
		res, err := c.http.Do(req)
		if err != nil {
			t.Fatal(SafeError(err))
		}
		data, err = io.ReadAll(res.Body)
		_ = res.Body.Close()
		if err != nil {
			t.Fatal(SafeError(err))
		}
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			t.Fatalf("presigned %s status %d", method, res.StatusCode)
		}
		if method == "GET" && string(data) != payload {
			t.Fatal("signed download differs")
		}
	}
	if err = c.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Get(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete result: %v", err)
	}
}
