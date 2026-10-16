/**
 * @Author: LiuShuXin
 * @Description: 验证真实连接的分块正文、空闲与写超时，以及监听异常回报
 * @File: network_test.go
 * Software: Goland
 * @Date: 2026/10/10 11:50
 */

package httpserver

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestNetworkChunkedBodyLimit(t *testing.T) {
	s, _ := testServer(t, testSettings())
	addr, err := s.Start()
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", addr.String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	// GET 路由不读取正文，仍须拒绝真实分块传输中超出限制的内容。
	_, err = io.WriteString(conn, "GET /sample HTTP/1.1\r\nHost: local\r\nTransfer-Encoding: chunked\r\n\r\n3\r\n123\r\n6\r\n456789\r\n0\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 413 || resp.Header.Get(requestIDHeader) == "" {
		t.Fatalf("chunked status=%d headers=%v", resp.StatusCode, resp.Header)
	}
}

func TestNetworkIdleTimeout(t *testing.T) {
	settings := testSettings()
	settings.IdleTimeout = 80 * time.Millisecond
	s, _ := testServer(t, settings)
	addr, err := s.Start()
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", addr.String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.WriteString(conn, "GET /sample HTTP/1.1\r\nHost: local\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || resp.Close {
		t.Fatal("first response did not retain a reusable connection")
	}
	_, err = reader.ReadByte()
	var networkError net.Error
	if err == nil || errors.As(err, &networkError) && networkError.Timeout() {
		t.Fatalf("idle connection was not closed by the server: %v", err)
	}
}

func TestOversizedUnfinishedBodyDoesNotDelayRejection(t *testing.T) {
	s, _ := testServer(t, testSettings())
	addr, err := s.Start()
	if err != nil {
		t.Fatal(err)
	}
	for _, framing := range []string{
		"Transfer-Encoding: chunked\r\n\r\n1000\r\n123456789",
		"Content-Length: 4096\r\n\r\n123456789",
	} {
		conn, err := net.DialTimeout("tcp", addr.String(), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		// 只发送超限部分；服务应及时拒绝，不等待客户端补齐正文。
		if _, err := io.WriteString(conn, "GET /sample HTTP/1.1\r\nHost: local\r\n"+framing); err != nil {
			t.Fatal(err)
		}
		reader := bufio.NewReader(conn)
		resp, err := http.ReadResponse(reader, nil)
		if err != nil {
			t.Fatalf("oversized unfinished body was not promptly rejected: %v", err)
		}
		if resp.StatusCode != 413 || !resp.Close {
			t.Fatalf("status=%d close=%t", resp.StatusCode, resp.Close)
		}
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		_, err = reader.ReadByte()
		var networkError net.Error
		if err == nil || errors.As(err, &networkError) && networkError.Timeout() {
			t.Fatalf("rejected request retained the connection: %v", err)
		}
	}
}

func TestNetworkWriteTimeout(t *testing.T) {
	settings := testSettings()
	settings.WriteTimeout = 150 * time.Millisecond
	s, _ := testServer(t, settings)
	s.Router().GET("/late", func(c *gin.Context) {
		// 写超时限制网络写入；它不会自行终止正在运行的 Handler。
		<-time.After(2 * settings.WriteTimeout)
		c.String(200, "too late")
	})
	addr, err := s.Start()
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + addr.String() + "/late")
	if err == nil {
		resp.Body.Close()
		t.Fatalf("expired write returned status %d", resp.StatusCode)
	}
	resp, err = client.Get("http://" + addr.String() + "/sample")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal("server did not survive write timeout")
	}
}

func TestUnexpectedListenerFailureIsReported(t *testing.T) {
	s, _ := testServer(t, testSettings())
	if _, err := s.Start(); err != nil {
		t.Fatal(err)
	}
	if err := s.listener.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-s.Errors():
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("unexpected listener failure was hidden: %v", err)
		}
		if s.ready.Load() {
			t.Fatal("failed server remains ready")
		}
	case <-time.After(time.Second):
		t.Fatal("serve failure was not reported")
	}
}
