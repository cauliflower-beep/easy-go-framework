/**
 * @Author: LiuShuXin
 * @Description: 无外部依赖验证应用装配、配置校验及信号取消后的资源回收
 * @File: main_test.go
 * Software: Goland
 * @Date: 2026/10/15
 */
package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func clearExampleEnvironment(t *testing.T) {
	t.Helper()
	for _, variable := range os.Environ() {
		key, _, _ := strings.Cut(variable, "=")
		if strings.HasPrefix(key, "EXAMPLE_") {
			t.Setenv(key, "")
			if err := os.Unsetenv(key); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestApplicationAssemblyAndShutdown(t *testing.T) {
	clearExampleEnvironment(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("http:\n  port: 0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := run(ctx, path, io.Discard, io.Discard); err != nil {
		t.Fatalf("dependency-free app did not assemble and stop: %v", err)
	}
	if err := os.WriteFile(path, []byte("http:\n  port: -1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := run(ctx, path, io.Discard, io.Discard); err == nil {
		t.Fatal("application did not validate loaded config")
	}
}

// 日志提供实际绑定地址，测试使用随机端口，不依赖机器的 8080 是否空闲。
type startupOutput struct{ addresses chan string }

func (w startupOutput) Write(data []byte) (int, error) {
	var entry struct {
		Message string `json:"message"`
		Address string `json:"address"`
	}
	if json.Unmarshal(data, &entry) == nil && entry.Message == "example server started" {
		select {
		case w.addresses <- entry.Address:
		default:
		}
	}
	return len(data), nil
}

func TestApplicationHTTPRoundTripAndGracefulShutdown(t *testing.T) {
	clearExampleEnvironment(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("http:\n  port: 0\nlog:\n  format: json\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	output := startupOutput{addresses: make(chan string, 1)}
	finished := make(chan error, 1)
	go func() { finished <- run(ctx, path, output, io.Discard) }()
	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	var address string
	select {
	case address = <-output.addresses:
	case err := <-finished:
		t.Fatalf("application exited before startup: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("application did not start")
	}
	for _, path := range []string{"/example", "/healthz", "/readyz"} {
		response, err := client.Get("http://" + address + path)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if readErr != nil || response.StatusCode != 200 || response.Header.Get("X-Request-ID") == "" || !json.Valid(body) {
			t.Fatalf("%s did not return a valid correlated response: %d / %v", path, response.StatusCode, readErr)
		}
	}
	cancel()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("application shutdown failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("application did not finish graceful shutdown")
	}
}
