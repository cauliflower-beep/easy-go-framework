/**
 * @Author: LiuShuXin
 * @Description: 下载流取消、超时、并发关闭与上传资源所有权验证
 * @File: lifecycle_test.go
 * Software: Goland
 * @Date: 2026/10/15
 */
package objectstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func streamHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Length", "100")
	fmt.Fprint(w, "x")
	w.(http.Flusher).Flush()
	<-r.Context().Done()
}

func waitClosing(t *testing.T, c *Client) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		closing := c.closing
		c.mu.Unlock()
		if closing {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("shutdown did not start")
}

func TestShutdownWaitsForDownload(t *testing.T) {
	for _, provider := range []string{ProviderS3, ProviderOSS} {
		t.Run(provider, func(t *testing.T) {
			c := testClient(t, provider, streamHandler, nil)
			obj, err := c.Get(context.Background(), "stream")
			if err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			fail := make(chan error, 8)
			for range 8 {
				wg.Go(func() { fail <- c.Shutdown(context.Background()) })
			}
			waitClosing(t, c)
			select {
			case err := <-fail:
				t.Fatalf("shutdown returned with active body: %v", err)
			default:
			}
			if _, err = c.Stat(context.Background(), "key"); !errors.Is(err, ErrClosed) {
				t.Fatalf("admission %v", err)
			}
			if _, err = c.PresignGet(context.Background(), "key", time.Minute); !errors.Is(err, ErrClosed) {
				t.Fatalf("presign admission %v", err)
			}
			if err = obj.Body.Close(); err != nil {
				t.Fatal(err)
			}
			wg.Wait()
			close(fail)
			for err := range fail {
				if err != nil {
					t.Error(err)
				}
			}
		})
	}
}

func TestDownloadTimeoutAndCancellation(t *testing.T) {
	for _, provider := range []string{ProviderS3, ProviderOSS} {
		for _, kind := range []string{"operation timeout", "parent cancel", "forced close", "shutdown timeout"} {
			t.Run(provider+"/"+kind, func(t *testing.T) {
				c := testClient(t, provider, streamHandler, func(o *Options) {
					if kind == "operation timeout" {
						o.OperationTimeout = 150 * time.Millisecond
					}
					if kind == "shutdown timeout" {
						o.HTTP.ShutdownTimeout = 60 * time.Millisecond
					}
				})
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				obj, err := c.Get(ctx, "stream")
				if err != nil {
					t.Fatal(err)
				}
				result := make(chan error, 1)
				go func() { _, err := io.ReadAll(obj.Body); result <- err }()
				switch kind {
				case "parent cancel":
					cancel()
				case "forced close":
					if err = c.Close(); err != nil {
						t.Fatal(err)
					}
				case "shutdown timeout":
					err = c.Shutdown(context.Background())
					if !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("shutdown deadline %v", err)
					}
				}
				select {
				case err := <-result:
					expected := context.Canceled
					if kind == "operation timeout" {
						expected = context.DeadlineExceeded
					}
					if !errors.Is(err, expected) {
						t.Fatalf("read cancellation %v, expected %v", err, expected)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("read did not exit")
				}
				_ = obj.Body.Close()
			})
		}
	}
}

func TestHeaderTimeoutAndCanceledContext(t *testing.T) {
	for _, provider := range []string{ProviderS3, ProviderOSS} {
		t.Run(provider, func(t *testing.T) {
			c := testClient(t, provider, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }, func(o *Options) { o.OperationTimeout = 100 * time.Millisecond })
			_, err := c.Get(context.Background(), "key")
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("headers deadline %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, err = c.Stat(ctx, "key")
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("parent cancellation %v", err)
			}
			if _, err = c.Get(nil, "key"); err == nil {
				t.Fatal("nil context accepted")
			}
		})
	}
}

func TestLargeAndEmptyObjects(t *testing.T) {
	for _, provider := range []string{ProviderS3, ProviderOSS} {
		for _, size := range []int{0, 9 << 20} {
			t.Run(fmt.Sprintf("%s/%d", provider, size), func(t *testing.T) {
				c := testClient(t, provider, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Length", fmt.Sprint(size))
					if r.Method == "GET" {
						_, _ = io.CopyN(w, strings.NewReader(strings.Repeat("x", size)), int64(size))
					}
				}, nil)
				obj, err := c.Get(context.Background(), "file")
				if err != nil {
					t.Fatal(err)
				}
				n, err := io.Copy(io.Discard, obj.Body)
				if err != nil || n != int64(size) {
					t.Fatalf("read %d %v", n, err)
				}
				_ = obj.Body.Close()
			})
		}
	}
}

func TestPutDoesNotCloseFile(t *testing.T) {
	for _, provider := range []string{ProviderS3, ProviderOSS} {
		t.Run(provider, func(t *testing.T) {
			c := testClient(t, provider, func(w http.ResponseWriter, r *http.Request) {
				data, _ := io.ReadAll(r.Body)
				w.Header().Set("x-oss-hash-crc64ecma", crc(data))
			}, nil)
			f, err := os.CreateTemp(t.TempDir(), "upload")
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			_, _ = f.WriteString("content")
			_, _ = f.Seek(0, io.SeekStart)
			_, err = c.Put(context.Background(), PutInput{Key: "file", Body: f})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.Seek(0, io.SeekStart); err != nil {
				t.Fatalf("caller file closed: %v", err)
			}
		})
	}
}

func TestConcurrentOperations(t *testing.T) {
	for _, provider := range []string{ProviderS3, ProviderOSS} {
		t.Run(provider, func(t *testing.T) {
			c := testClient(t, provider, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", "1")
				if r.Method == "GET" {
					fmt.Fprint(w, "x")
				}
			}, nil)
			var wg sync.WaitGroup
			for range 24 {
				wg.Go(func() {
					obj, err := c.Get(context.Background(), "key")
					if err != nil {
						t.Error(err)
						return
					}
					_, err = io.Copy(io.Discard, obj.Body)
					_ = obj.Body.Close()
					if err != nil {
						t.Error(err)
					}
				})
			}
			wg.Wait()
			if err := c.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
