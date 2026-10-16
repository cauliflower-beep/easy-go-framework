/**
 * @Author: LiuShuXin
 * @Description: 用受控 TCP 夹具验证 Redis 异常、取消、关闭与写命令不重放
 * @File: client_test.go
 * Software: Goland
 * @Date: 2026/10/14
 */
package redis

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cauliflower-beep/easy-go-framework/logging"
	goredis "github.com/redis/go-redis/v9"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// 夹具只实现连接握手及故障注入，不替代真实 Redis 命令语义验证。
func replyFixture(t *testing.T, reply func(net.Conn, []string)) Options {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return replyFixtureOn(t, listener, reply)
}

func replyFixtureOn(t *testing.T, listener net.Listener, reply func(net.Conn, []string)) Options {
	t.Helper()
	var mu sync.Mutex
	connections := map[net.Conn]bool{}
	var workers sync.WaitGroup
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			connections[conn] = true
			workers.Add(1)
			mu.Unlock()
			go func() {
				defer workers.Done()
				defer func() { conn.Close(); mu.Lock(); delete(connections, conn); mu.Unlock() }()
				reader := bufio.NewReader(conn)
				for {
					args, err := readFixtureCommand(reader)
					if err != nil {
						return
					}
					if strings.EqualFold(args[0], "hello") {
						io.WriteString(conn, "*4\r\n$6\r\nserver\r\n$5\r\nredis\r\n$5\r\nproto\r\n:2\r\n")
						continue
					}
					if strings.EqualFold(args[0], "ping") {
						io.WriteString(conn, "+PONG\r\n")
						continue
					}
					if strings.EqualFold(args[0], "client") || strings.EqualFold(args[0], "select") {
						io.WriteString(conn, "+OK\r\n")
						continue
					}
					reply(conn, args)
				}
			}()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		<-done
		mu.Lock()
		for conn := range connections {
			conn.Close()
		}
		mu.Unlock()
		workers.Wait()
	})
	o := DefaultOptions()
	o.TLS = false
	o.Port = listener.Addr().(*net.TCPAddr).Port
	o.ConnectTimeout = time.Second
	o.CommandTimeout = time.Second
	o.ReadTimeout = 300 * time.Millisecond
	o.WriteTimeout = 300 * time.Millisecond
	o.HealthTimeout = time.Second
	o.ShutdownTimeout = time.Second
	return o
}

func readFixtureCommand(reader *bufio.Reader) ([]string, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if len(line) < 3 || line[0] != '*' {
		return nil, errors.New("invalid fixture command")
	}
	n, err := strconv.Atoi(strings.TrimSpace(line[1:]))
	if err != nil || n < 1 || n > 128 {
		return nil, errors.New("invalid fixture count")
	}
	args := make([]string, n)
	for i := range n {
		line, err = reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if len(line) < 3 || line[0] != '$' {
			return nil, errors.New("invalid fixture bulk")
		}
		size, err := strconv.Atoi(strings.TrimSpace(line[1:]))
		if err != nil || size < 0 || size > 1<<20 {
			return nil, errors.New("invalid fixture size")
		}
		data := make([]byte, size+2)
		if _, err = io.ReadFull(reader, data); err != nil {
			return nil, err
		}
		args[i] = string(data[:size])
	}
	return args, nil
}

func fixtureClient(t *testing.T, o Options, logger *zap.Logger) *Client {
	t.Helper()
	c, err := Open(context.Background(), o, logger)
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

func TestCommandFailureDoesNotReplayWrite(t *testing.T) {
	var writes atomic.Int32
	o := replyFixture(t, func(conn net.Conn, args []string) {
		if strings.EqualFold(args[0], "incr") {
			writes.Add(1)
			conn.Close()
		}
	})
	c := fixtureClient(t, o, zap.NewNop())
	err := c.WithClient(context.Background(), func(ctx context.Context, rdb goredis.Cmdable) error { return rdb.Incr(ctx, "sensitive-key").Err() })
	if err == nil || writes.Load() != 1 {
		t.Fatalf("write replayed or missing failure: %v count=%d", err, writes.Load())
	}
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("subsequent operation did not reconnect: %v", err)
	}
}

func TestDeadlineCancellationAndClose(t *testing.T) {
	o := replyFixture(t, func(net.Conn, []string) {})
	c := fixtureClient(t, o, zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.WithClient(ctx, func(context.Context, goredis.Cmdable) error { t.Error("canceled callback ran"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := c.WithClient(ctx, func(ctx context.Context, rdb goredis.Cmdable) error { return rdb.Get(ctx, "sensitive-key").Err() })
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("deadline not propagated: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Ping(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatal("closed client admitted operation")
	}
	if err := c.WithClient(nil, func(context.Context, goredis.Cmdable) error { return nil }); err == nil {
		t.Fatal("nil context accepted")
	}
}

func TestCloseCancelsIOAndIsConcurrent(t *testing.T) {
	entered := make(chan struct{}, 1)
	o := replyFixture(t, func(net.Conn, []string) { entered <- struct{}{} })
	o.ReadTimeout = 5 * time.Second
	o.CommandTimeout = 5 * time.Second
	c := fixtureClient(t, o, zap.NewNop())
	done := make(chan error, 1)
	go func() {
		done <- c.WithClient(context.Background(), func(ctx context.Context, rdb goredis.Cmdable) error { return rdb.Get(ctx, "secret").Err() })
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("command did not reach fixture")
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.Close(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("close cancellation lost: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("close left socket IO running")
	}
}

func TestCloseTimeoutAndPanicCallback(t *testing.T) {
	o := replyFixture(t, func(net.Conn, []string) {})
	o.ShutdownTimeout = 20 * time.Millisecond
	c := fixtureClient(t, o, zap.NewNop())
	func() {
		defer func() {
			if recover() != "fixture panic" {
				t.Error("panic did not propagate")
			}
		}()
		c.WithClient(context.Background(), func(context.Context, goredis.Cmdable) error { panic("fixture panic") })
	}()
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- c.WithClient(context.Background(), func(context.Context, goredis.Cmdable) error { close(entered); <-release; return nil })
	}()
	<-entered
	err := c.Close()
	close(release)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("non-cooperative callback did not time out: %v", err)
	}
	if !errors.Is(<-done, context.Canceled) {
		t.Fatal("callback cancel lost")
	}
	select {
	case <-c.closed:
	case <-time.After(time.Second):
		t.Fatal("resources did not finish closing")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

type fixtureServerError string

func (e fixtureServerError) Error() string { return string(e) }
func (fixtureServerError) RedisError()     {}

func TestErrorClassificationAndPrivacy(t *testing.T) {
	for _, test := range []struct {
		cause  error
		target error
	}{
		{goredis.Nil, Nil}, {context.Canceled, context.Canceled}, {context.DeadlineExceeded, context.DeadlineExceeded}, {goredis.ErrPoolTimeout, ErrPoolTimeout}, {goredis.ErrClosed, ErrClosed},
		{fixtureServerError("WRONGPASS private-password"), ErrAuthentication}, {fixtureServerError("NOPERM private-key"), ErrPermission},
	} {
		err := SafeError(test.cause)
		if !errors.Is(err, test.target) {
			t.Fatalf("classification lost: %v", err)
		}
		fields, _ := logging.Diagnostics(err)
		if strings.Contains(fmt.Sprint(fields), "private") {
			t.Fatal("diagnostic leaked")
		}
	}
	cause := fixtureServerError("ERR secret script body")
	err := SafeError(cause)
	var server goredis.Error
	if !errors.As(err, &server) || server.Error() != cause.Error() || strings.Contains(fmt.Sprintf("%+v %#v", err, err), "secret") {
		t.Fatal("error chain or formatting unsafe")
	}
	if SafeError(nil) != nil || SafeError(err) != err {
		t.Fatal("SafeError is not idempotent")
	}
}

func TestHookProtectsCommandAndPipelineErrors(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	o := replyFixture(t, func(conn net.Conn, args []string) {
		io.WriteString(conn, "-ERR sensitive-key sensitive-value private-password\r\n")
	})
	c := fixtureClient(t, o, zap.New(core))
	ctx := logging.WithRequestID(context.Background(), "fixture-request")
	var commands []goredis.Cmder
	err := c.WithClient(ctx, func(ctx context.Context, rdb goredis.Cmdable) error {
		if commandErr := rdb.Get(ctx, "sensitive-key").Err(); commandErr == nil || strings.Contains(commandErr.Error(), "sensitive") {
			t.Error("raw command error exposed")
		}
		var err error
		commands, err = rdb.Pipelined(ctx, func(pipe goredis.Pipeliner) error {
			pipe.Get(ctx, "sensitive-key")
			pipe.Set(ctx, "other-key", "sensitive-value", 0)
			return nil
		})
		return err
	})
	if err == nil || strings.Contains(err.Error(), "sensitive") {
		t.Fatal("unsafe pipeline return")
	}
	for _, cmd := range commands {
		if cmd.Err() == nil || strings.Contains(cmd.Err().Error(), "sensitive") {
			t.Fatal("unsafe pipeline command return")
		}
	}
	for _, entry := range logs.All() {
		for _, field := range entry.Context {
			if strings.Contains(fmt.Sprint(field), "sensitive") || strings.Contains(fmt.Sprint(field), "private-password") {
				t.Fatal("command data logged")
			}
		}
	}
	if logs.FilterMessage("Redis command failed").Len() != 2 || logs.FilterField(zap.String("request_id", "fixture-request")).Len() != 2 {
		t.Fatalf("command diagnostics=%d request correlation=%d logs=%v", logs.FilterMessage("Redis command failed").Len(), logs.FilterField(zap.String("request_id", "fixture-request")).Len(), logs.All())
	}
	entries := logs.FilterMessage("Redis command failed").All()
	if entries[0].ContextMap()["command"] != "GET" || entries[1].ContextMap()["command_types"] == nil {
		t.Fatal("real command or pipeline diagnostic missing command identity")
	}
}

func TestStartupFailureIsBounded(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	closed := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		io.Copy(io.Discard, conn)
		close(closed)
	}()
	o := DefaultOptions()
	o.TLS = false
	o.Port = listener.Addr().(*net.TCPAddr).Port
	o.ConnectTimeout = 40 * time.Millisecond
	c, err := Open(context.Background(), o, zap.NewNop())
	if c != nil || err == nil {
		t.Fatal("startup handshake failure ignored")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("failed startup leaked connection")
	}
	if c, err := Open(nil, o, zap.NewNop()); c != nil || err == nil {
		t.Fatal("nil startup context accepted")
	}
}
