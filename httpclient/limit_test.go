/**
 * @Author: LiuShuXin
 * @Description: 验证响应体、解压后字节及响应头上限，支持显式流式覆盖
 * @File: limit_test.go
 * Software: Goland
 * @Date: 2026/10/14
 */
package httpclient

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResponseSizeLimits(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/known":
			w.Header().Set("Content-Length", "5")
			io.WriteString(w, "12345")
		case "/exact":
			io.WriteString(w, "1234")
		case "/gzip":
			w.Header().Set("Content-Encoding", "gzip")
			writer := gzip.NewWriter(w)
			io.WriteString(writer, strings.Repeat("a", 200))
			writer.Close()
		default:
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			io.WriteString(w, "12345")
		}
	}))
	t.Cleanup(server.Close)
	o := DefaultOptions()
	o.MaxResponseBytes = 4
	c := testClient(t, o)
	if _, err := c.Do(testRequest(t, context.Background(), "GET", server.URL+"/known", nil)); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("declared overflow not rejected: %v", err)
	}
	for _, path := range []string{"/chunked", "/gzip"} {
		response, err := c.Do(testRequest(t, context.Background(), "GET", server.URL+path, nil))
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(response.Body)
		response.Body.Close()
		if !errors.Is(err, ErrResponseTooLarge) || len(data) > 4 {
			t.Fatalf("decoded or chunked overflow not bounded: %v bytes=%d", err, len(data))
		}
	}
	response, err := c.Do(testRequest(t, context.Background(), "GET", server.URL+"/exact", nil))
	if err != nil || readResponse(t, response) != "1234" {
		t.Fatalf("exact-limit response rejected: %v", err)
	}
	var probe [1]byte
	if _, err := response.Body.Read(probe[:]); err != io.EOF {
		t.Fatal("completed body did not preserve repeated EOF")
	}
	response, err = c.DoWithOptions(testRequest(t, context.Background(), "GET", server.URL+"/known", nil), RequestOptions{MaxResponseBytes: -1})
	if err != nil || readResponse(t, response) != "12345" {
		t.Fatalf("explicit streaming size override failed: %v", err)
	}
	if _, err := c.DoWithOptions(testRequest(t, context.Background(), "GET", server.URL+"/exact", nil), RequestOptions{MaxResponseBytes: 2}); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("positive per-request size override ignored: %v", err)
	}
	response, err = c.Do(testRequest(t, context.Background(), "HEAD", server.URL+"/known", nil))
	if err != nil || readResponse(t, response) != "" {
		t.Fatalf("HEAD representation length counted as response bytes: %v", err)
	}
}

func TestResponseHeaderLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Large", strings.Repeat("a", 4096))
		w.WriteHeader(200)
	}))
	t.Cleanup(server.Close)
	o := DefaultOptions()
	o.MaxResponseHeaderBytes = 1024
	c := testClient(t, o)
	if response, err := c.Do(testRequest(t, context.Background(), "GET", server.URL, nil)); err == nil {
		response.Body.Close()
		t.Fatal("oversized headers accepted")
	}
}
