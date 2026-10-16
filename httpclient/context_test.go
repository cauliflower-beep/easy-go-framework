/**
 * @Author: LiuShuXin
 * @Description: 验证出站 HTTP 分阶段超时、调用方取消、流式期限及连接等待
 * @File: context_test.go
 * Software: Goland
 * @Date: 2026/10/14
 */
package httpclient

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHeaderAndBodyDeadlines(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/body" {
			io.WriteString(w, "a")
			w.(http.Flusher).Flush()
		}
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	for _, phase := range []string{"headers", "body", "caller cancellation"} {
		t.Run(phase, func(t *testing.T) {
			o := DefaultOptions()
			o.RequestTimeout = time.Second
			o.ResponseHeaderTimeout = time.Second
			if phase == "headers" {
				o.ResponseHeaderTimeout = 60 * time.Millisecond
			} else if phase == "body" {
				o.RequestTimeout = 80 * time.Millisecond
			}
			c := testClient(t, o)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			path := "/body"
			if phase == "headers" {
				path = "/headers"
			}
			start := time.Now()
			response, err := c.Do(testRequest(t, ctx, "GET", server.URL+path, nil))
			if phase == "headers" {
				var network net.Error
				if !errors.As(err, &network) || !network.Timeout() {
					t.Fatalf("response-header timeout classification lost: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if phase == "caller cancellation" {
					cancel()
				}
				_, err = io.ReadAll(response.Body)
				response.Body.Close()
				target := context.DeadlineExceeded
				if phase == "caller cancellation" {
					target = context.Canceled
				}
				if !errors.Is(err, target) {
					t.Fatalf("body read lost context failure: %v", err)
				}
			}
			if time.Since(start) > 600*time.Millisecond {
				t.Fatal("request did not respect shorter deadline")
			}
		})
	}
}

func TestPerRequestTimeoutAndCallerDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(150 * time.Millisecond):
			io.WriteString(w, "done")
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(server.Close)
	o := DefaultOptions()
	o.RequestTimeout = 60 * time.Millisecond
	c := testClient(t, o)
	if _, err := c.Do(testRequest(t, context.Background(), "GET", server.URL, nil)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("default request deadline not applied: %v", err)
	}
	response, err := c.DoWithOptions(testRequest(t, context.Background(), "GET", server.URL, nil), RequestOptions{Timeout: time.Second})
	if err != nil || readResponse(t, response) != "done" {
		t.Fatalf("per-request override did not extend default: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if _, err := c.DoWithOptions(testRequest(t, ctx, "GET", server.URL, nil), RequestOptions{Timeout: time.Second}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("override extended caller deadline: %v", err)
	}
}

func TestStreamingResponseSurvivesDoReturnAndDefaultBudget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "first-")
		w.(http.Flusher).Flush()
		select {
		case <-time.After(150 * time.Millisecond):
			io.WriteString(w, "last")
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(server.Close)
	o := DefaultOptions()
	o.RequestTimeout = 60 * time.Millisecond
	o.ResponseHeaderTimeout = 100 * time.Millisecond
	c := testClient(t, o)
	start := time.Now()
	response, err := c.DoWithOptions(testRequest(t, context.Background(), "GET", server.URL, nil), RequestOptions{Timeout: time.Second, MaxResponseBytes: -1})
	if err != nil {
		t.Fatal(err)
	}
	var first [6]byte
	if _, err := io.ReadFull(response.Body, first[:]); err != nil || string(first[:]) != "first-" {
		t.Fatalf("first stream chunk unavailable before completion: %v", err)
	}
	if got := readResponse(t, response); got != "last" || time.Since(start) < 100*time.Millisecond {
		t.Fatalf("stream not preserved beyond Do/default/header deadline: %q", got)
	}
}

func TestConnectionWaitRespectsCallerDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/block" {
			io.WriteString(w, "a")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		io.WriteString(w, "ok")
	}))
	t.Cleanup(server.Close)
	o := DefaultOptions()
	o.MaxConnsPerHost, o.MaxIdleConnsPerHost = 1, 1
	c := testClient(t, o)
	response, err := c.Do(testRequest(t, context.Background(), "GET", server.URL+"/block", nil))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if _, err := c.Do(testRequest(t, ctx, "GET", server.URL+"/ok", nil)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("connection wait ignored caller deadline: %v", err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	response, err = c.Do(testRequest(t, context.Background(), "GET", server.URL+"/ok", nil))
	if err != nil || readResponse(t, response) != "ok" {
		t.Fatalf("closing partial body did not release connection slot: %v", err)
	}
}
