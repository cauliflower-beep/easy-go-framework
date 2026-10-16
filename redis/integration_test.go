/**
 * @Author: LiuShuXin
 * @Description: 使用真实 Redis 验证读写、TTL、Pipeline、认证和连接池行为
 * @File: integration_test.go
 * Software: Goland
 * @Date: 2026/10/14
 */
package redis

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

func integrationOptions(t *testing.T) Options {
	t.Helper()
	host := os.Getenv("REDIS_TEST_HOST")
	if host == "" {
		t.Skip("real Redis integration requires REDIS_TEST_HOST")
	}
	o := DefaultOptions()
	o.Host = host
	o.User = os.Getenv("REDIS_TEST_USER")
	o.Password = os.Getenv("REDIS_TEST_PASSWORD")
	o.TLSRootCert = os.Getenv("REDIS_TEST_TLS_ROOT_CERT")
	var err error
	o.TLS, err = strconv.ParseBool(os.Getenv("REDIS_TEST_TLS"))
	if err != nil {
		t.Fatal("REDIS_TEST_TLS must be explicit true or false")
	}
	o.Database, err = strconv.Atoi(os.Getenv("REDIS_TEST_DATABASE"))
	if err != nil {
		t.Fatal("REDIS_TEST_DATABASE must be explicit")
	}
	if port := os.Getenv("REDIS_TEST_PORT"); port != "" {
		o.Port, err = strconv.Atoi(port)
		if err != nil {
			t.Fatal("invalid test port")
		}
	}
	return o
}

func integrationPrefix(t *testing.T) string {
	t.Helper()
	var bytes [12]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		t.Fatal(err)
	}
	return "framework-test:" + hex.EncodeToString(bytes[:]) + ":"
}

func TestIntegrationReadWriteTTLAndPipelines(t *testing.T) {
	o := integrationOptions(t)
	c := fixtureClient(t, o, zap.NewNop())
	prefix := integrationPrefix(t)
	keys := []string{prefix + "value", prefix + "count", prefix + "missing"}
	t.Cleanup(func() {
		if err := c.WithClient(context.Background(), func(ctx context.Context, rdb goredis.Cmdable) error { return rdb.Del(ctx, keys...).Err() }); err != nil {
			t.Error(err)
		}
	})
	if err := c.WithClient(context.Background(), func(ctx context.Context, rdb goredis.Cmdable) error {
		if err := rdb.Set(ctx, keys[0], "integration-value", time.Minute).Err(); err != nil {
			return err
		}
		value, err := rdb.Get(ctx, keys[0]).Result()
		if err != nil {
			return err
		}
		if value != "integration-value" {
			t.Error("unexpected real value")
		}
		ttl, err := rdb.TTL(ctx, keys[0]).Result()
		if err != nil {
			return err
		}
		if ttl <= 0 || ttl > time.Minute {
			t.Error("real TTL missing")
		}
		if err := rdb.PExpire(ctx, keys[0], 50*time.Millisecond).Err(); err != nil {
			return err
		}
		deadline := time.Now().Add(time.Second)
		for !errors.Is(rdb.Get(ctx, keys[0]).Err(), Nil) {
			if time.Now().After(deadline) {
				t.Error("real key did not expire")
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err := rdb.Get(ctx, keys[2]).Err(); !errors.Is(err, Nil) {
			t.Error("missing key lost Nil classification")
		}
		commands, err := rdb.Pipelined(ctx, func(pipe goredis.Pipeliner) error {
			pipe.Set(ctx, keys[1], 0, time.Minute)
			pipe.Incr(ctx, keys[1])
			return nil
		})
		if err != nil {
			return err
		}
		if len(commands) != 2 {
			t.Error("pipeline result count")
		}
		_, err = rdb.TxPipelined(ctx, func(pipe goredis.Pipeliner) error {
			pipe.Incr(ctx, keys[1])
			pipe.Expire(ctx, keys[1], time.Minute)
			return nil
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.WithClient(context.Background(), func(ctx context.Context, rdb goredis.Cmdable) error { return rdb.Incr(ctx, keys[1]).Err() }); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if err := c.WithClient(context.Background(), func(ctx context.Context, rdb goredis.Cmdable) error {
		value, err := rdb.Get(ctx, keys[1]).Int()
		if err == nil && value != 18 {
			t.Errorf("concurrent updates=%d want=18", value)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if stats := c.Stats(); stats.TotalConns == 0 || stats.TotalConns > uint32(o.PoolSize) {
		t.Fatalf("invalid pool ceiling: %+v", stats)
	}
}

func TestIntegrationAuthenticationAndServerErrorPrivacy(t *testing.T) {
	o := integrationOptions(t)
	c := fixtureClient(t, o, zap.NewNop())
	if o.Password != "" {
		bad := o
		bad.Password = "intentionally-invalid-test-password"
		client, err := Open(context.Background(), bad, zap.NewNop())
		if client != nil {
			client.Close()
			t.Fatal("wrong password accepted")
		}
		if !errors.Is(err, ErrAuthentication) || strings.Contains(err.Error(), bad.Password) {
			t.Fatalf("authentication failure unsafe: %v", err)
		}
	} else {
		t.Log("authentication rejection not checked: server credentials not configured")
	}
	err := c.WithClient(context.Background(), func(ctx context.Context, rdb goredis.Cmdable) error {
		return rdb.Eval(ctx, "return redis.error_reply('ERR private-script-payload')", nil).Err()
	})
	if err == nil || strings.Contains(err.Error(), "private-script-payload") {
		t.Fatal("server error payload exposed")
	}
	var server goredis.Error
	if !errors.As(err, &server) {
		t.Fatal("server error chain lost")
	}
}

func TestIntegrationPoolWaitAndCloseCancellation(t *testing.T) {
	o := integrationOptions(t)
	o.PoolSize = 1
	o.PoolTimeout = 40 * time.Millisecond
	c := fixtureClient(t, o, zap.NewNop())
	key := integrationPrefix(t) + "block"
	done := make(chan error, 1)
	go func() {
		done <- c.WithClient(context.Background(), func(ctx context.Context, rdb goredis.Cmdable) error { return rdb.BLPop(ctx, 3*time.Second, key).Err() })
	}()
	deadline := time.Now().Add(time.Second)
	for c.Stats().IdleConns != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if c.Stats().IdleConns != 0 {
		t.Fatal("blocking command did not acquire connection")
	}
	start := time.Now()
	err := c.Ping(context.Background())
	if !errors.Is(err, ErrPoolTimeout) || time.Since(start) > time.Second {
		t.Fatalf("pool wait not bounded: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("in-flight cancel lost: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocking command survived close")
	}
	if err := c.Ping(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatal("closed client admitted Ping")
	}
}

func TestIntegrationBlockingBudgetAndGracefulShutdown(t *testing.T) {
	o := integrationOptions(t)
	producer := fixtureClient(t, o, zap.NewNop())
	o.CommandTimeout = 40 * time.Millisecond
	o.PoolSize = 1
	c := fixtureClient(t, o, zap.NewNop())
	key := integrationPrefix(t) + "graceful-block"
	t.Cleanup(func() {
		if err := producer.WithClient(context.Background(), func(ctx context.Context, cmd goredis.Cmdable) error {
			return cmd.Del(ctx, key).Err()
		}); err != nil {
			t.Error(err)
		}
	})
	// 显式期限允许 Redis 自然结束一秒阻塞，而非被默认 40ms 回调期限截断。
	start := time.Now()
	err := c.WithClientTimeout(context.Background(), 3*time.Second, func(ctx context.Context, cmd goredis.Cmdable) error {
		return cmd.BLPop(ctx, time.Second, key).Err()
	})
	if !errors.Is(err, Nil) || errors.Is(err, context.DeadlineExceeded) || time.Since(start) < 800*time.Millisecond {
		t.Fatalf("blocking override did not preserve server timeout: %v", err)
	}
	operation := make(chan error, 1)
	go func() {
		operation <- c.WithClientTimeout(context.Background(), 3*time.Second, func(ctx context.Context, cmd goredis.Cmdable) error {
			values, err := cmd.BLPop(ctx, 2*time.Second, key).Result()
			if err == nil && (len(values) != 2 || values[1] != "delivered") {
				return errors.New("blocking consumer returned unexpected value")
			}
			return err
		})
	}()
	deadline := time.Now().Add(time.Second)
	for c.Stats().IdleConns != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if c.Stats().IdleConns != 0 {
		t.Fatal("blocking command did not acquire connection")
	}
	shutdown := make(chan error, 1)
	go func() { shutdown <- c.Shutdown(context.Background()) }()
	waitForDrain(t, c)
	if err := c.Ping(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("shutdown admitted health check: %v", err)
	}
	// 数据在默认期限之后到达，排空中的已准入消费者仍应成功完成。
	time.Sleep(120 * time.Millisecond)
	if err := producer.WithClient(context.Background(), func(ctx context.Context, cmd goredis.Cmdable) error {
		return cmd.RPush(ctx, key, "delivered").Err()
	}); err != nil {
		t.Fatal(err)
	}
	if err := awaitOperation(t, operation); err != nil {
		t.Fatalf("shutdown interrupted blocking consumer: %v", err)
	}
	if err := awaitOperation(t, shutdown); err != nil {
		t.Fatal(err)
	}
}
