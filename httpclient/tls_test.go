/**
 * @Author: LiuShuXin
 * @Description: 验证真实 TLS 信任、主机名、握手期限及 HTTP/2 协商
 * @File: tls_test.go
 * Software: Goland
 * @Date: 2026/10/14
 */
package httpclient

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func trustedOptions(t *testing.T, server *httptest.Server) Options {
	t.Helper()
	file := filepath.Join(t.TempDir(), "ca.pem")
	data := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	o := DefaultOptions()
	o.TLSRootCert = file
	return o
}

func TestTLSAndHTTP2(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "secure") }))
	server.EnableHTTP2 = true
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	t.Cleanup(server.Close)
	for _, enabled := range []bool{true, false} {
		o := trustedOptions(t, server)
		o.HTTP2 = enabled
		c := testClient(t, o)
		response, err := c.Do(testRequest(t, context.Background(), "GET", server.URL, nil))
		if err != nil {
			t.Fatal(err)
		}
		expected := 1
		if enabled {
			expected = 2
		}
		if response.ProtoMajor != expected || readResponse(t, response) != "secure" {
			t.Fatalf("unexpected TLS protocol: %s", response.Proto)
		}
	}
	c := testClient(t, DefaultOptions())
	_, err := c.Do(testRequest(t, context.Background(), "GET", server.URL+"/private?token=secret", nil))
	var unknown x509.UnknownAuthorityError
	if !errors.As(err, &unknown) || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("untrusted TLS accepted or URL exposed: %v", err)
	}
	c = testClient(t, trustedOptions(t, server))
	_, err = c.Do(testRequest(t, context.Background(), "GET", strings.Replace(server.URL, "127.0.0.1", "localhost", 1), nil))
	var hostname x509.HostnameError
	if !errors.As(err, &hostname) {
		t.Fatalf("TLS hostname mismatch not rejected: %v", err)
	}
}

func TestTLSHandshakeDeadline(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		connection, err := listener.Accept()
		if err == nil {
			defer connection.Close()
			io.Copy(io.Discard, connection)
		}
	}()
	t.Cleanup(func() { listener.Close(); <-done })
	o := DefaultOptions()
	o.TLSHandshakeTimeout = 60 * time.Millisecond
	c := testClient(t, o)
	start := time.Now()
	_, err = c.Do(testRequest(t, context.Background(), "GET", "https://"+listener.Addr().String(), nil))
	var network net.Error
	if !errors.As(err, &network) || !network.Timeout() || time.Since(start) > 700*time.Millisecond {
		t.Fatalf("TLS handshake wait not bounded: %v", err)
	}
}

func TestRedirectCannotDowngradeTLS(t *testing.T) {
	plainCalls := make(chan struct{}, 1)
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { plainCalls <- struct{}{} }))
	t.Cleanup(plain.Close)
	secure := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL, http.StatusFound)
	}))
	secure.Config.ErrorLog = log.New(io.Discard, "", 0)
	secure.StartTLS()
	t.Cleanup(secure.Close)
	o := trustedOptions(t, secure)
	o.MaxRedirects = 1
	c := testClient(t, o)
	if _, err := c.Do(testRequest(t, context.Background(), "GET", secure.URL, nil)); !errors.Is(err, ErrRedirectDowngrade) {
		t.Fatalf("TLS downgrade not rejected: %v", err)
	}
	select {
	case <-plainCalls:
		t.Fatal("insecure redirect target contacted")
	default:
	}
}

func TestHTTP2CancellationKeepsOtherStreamsAndConnection(t *testing.T) {
	var connections atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stream" {
			io.WriteString(w, "start")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		io.WriteString(w, "ok")
	}))
	server.EnableHTTP2 = true
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	o := trustedOptions(t, server)
	o.MaxConnsPerHost, o.MaxIdleConnsPerHost = 1, 1
	c := testClient(t, o)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := c.Do(testRequest(t, ctx, "GET", server.URL+"/stream", nil))
	if err != nil || stream.ProtoMajor != 2 {
		t.Fatalf("HTTP/2 stream not established: %v", err)
	}
	response, err := c.DoWithOptions(testRequest(t, context.Background(), "GET", server.URL+"/ok", nil), RequestOptions{Timeout: time.Second})
	if err != nil || readResponse(t, response) != "ok" {
		t.Fatalf("active stream prevented multiplexed request: %v", err)
	}
	cancel()
	if _, err := io.ReadAll(stream.Body); !errors.Is(err, context.Canceled) {
		t.Fatalf("HTTP/2 stream cancellation lost: %v", err)
	}
	stream.Body.Close()
	response, err = c.Do(testRequest(t, context.Background(), "GET", server.URL+"/ok", nil))
	if err != nil || readResponse(t, response) != "ok" || connections.Load() != 1 {
		t.Fatalf("canceling one stream closed shared connection: %v connections=%d", err, connections.Load())
	}
}
