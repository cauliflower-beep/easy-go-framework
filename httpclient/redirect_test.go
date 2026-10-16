/**
 * @Author: LiuShuXin
 * @Description: 验证默认重定向语义、同源重定向上限及凭证隔离
 * @File: redirect_test.go
 * Software: Goland
 * @Date: 2026/10/14
 */
package httpclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestRedirectPolicy(t *testing.T) {
	var finalCalls atomic.Int32
	var refererLeaked atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		finalCalls.Add(1)
	}))
	t.Cleanup(other.Close)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start":
			http.Redirect(w, r, "/final", http.StatusFound)
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusFound)
		case "/cross":
			http.Redirect(w, r, other.URL+"/private?token=secret", http.StatusTemporaryRedirect)
		default:
			if r.Header.Get("Referer") != "" {
				refererLeaked.Store(true)
			}
			io.WriteString(w, "final")
		}
	}))
	t.Cleanup(server.Close)
	c := testClient(t, DefaultOptions())
	response, err := c.Do(testRequest(t, context.Background(), "GET", server.URL+"/start", nil))
	if err != nil || response.StatusCode != 302 {
		t.Fatalf("default redirect followed or converted to error: %v", err)
	}
	readResponse(t, response)
	o := DefaultOptions()
	o.MaxRedirects = 1
	c = testClient(t, o)
	response, err = c.Do(testRequest(t, context.Background(), "GET", server.URL+"/start?private=secret", nil))
	if err != nil || readResponse(t, response) != "final" || refererLeaked.Load() {
		t.Fatalf("same-origin redirect failed or leaked Referer: %v", err)
	}
	if _, err := c.Do(testRequest(t, context.Background(), "GET", server.URL+"/loop", nil)); !errors.Is(err, ErrRedirectLimit) {
		t.Fatalf("redirect loop not bounded: %v", err)
	}
	req := testRequest(t, context.Background(), "POST", server.URL+"/cross", nil)
	req.Header.Set("Authorization", "Bearer secret")
	if _, err := c.Do(req); !errors.Is(err, ErrRedirectOrigin) || finalCalls.Load() != 0 {
		t.Fatalf("cross-origin request sent: %v count=%d", err, finalCalls.Load())
	}
}
