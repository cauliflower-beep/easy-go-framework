/**
 * @Author: LiuShuXin
 * @Description: 显式启用的真实 Redis 任务往返测试，默认不连接外部服务
 * @File: integration_test.go
 * Software: Goland
 * @Date: 2026/10/15
 */
package taskqueue

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"go.uber.org/zap"
)

func TestIntegrationTaskRoundTrip(t *testing.T) {
	if os.Getenv("TASKQUEUE_TEST_INTEGRATION") != "1" {
		t.Skip("real Redis task integration disabled; set TASKQUEUE_TEST_INTEGRATION=1 to opt in")
	}
	o := DefaultOptions()
	o.Redis.Host = os.Getenv("TASKQUEUE_TEST_HOST")
	o.Redis.User = os.Getenv("TASKQUEUE_TEST_USER")
	o.Redis.Password = os.Getenv("TASKQUEUE_TEST_PASSWORD")
	o.Redis.TLSRootCert = os.Getenv("TASKQUEUE_TEST_TLS_ROOT_CERT")
	var err error
	if o.Redis.Port, err = strconv.Atoi(os.Getenv("TASKQUEUE_TEST_PORT")); err != nil {
		t.Fatal("explicit test port required")
	}
	if o.Redis.Database, err = strconv.Atoi(os.Getenv("TASKQUEUE_TEST_DATABASE")); err != nil {
		t.Fatal("explicit dedicated test database required")
	}
	if o.Redis.TLS, err = strconv.ParseBool(os.Getenv("TASKQUEUE_TEST_TLS")); err != nil {
		t.Fatal("explicit test TLS flag required")
	}
	o.Queue = "framework-test-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	c, err := NewClient(o, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	if err = c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	// 只删除本次随机队列，不执行 FLUSH*，不枚举或移除其他应用队列。
	defer func() {
		inspector := asynq.NewInspectorFromRedisClient(c.raw)
		if err := inspector.DeleteQueue(o.Queue, false); err != nil && !errors.Is(err, asynq.ErrQueueNotFound) {
			t.Error(SafeError(err))
		}
	}()
	w, err := NewWorker(o, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	if err = w.Register("fixture.integration", func(_ context.Context, task Task) (json.RawMessage, error) { return task.Payload, nil }); err != nil {
		t.Fatal(err)
	}
	if err = w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := w.Close(); err != nil {
			t.Error(err)
		}
	}()
	id := uuid.NewString()
	defer func() {
		if err := w.Close(); err != nil {
			t.Error(err)
			return
		}
		inspector := asynq.NewInspectorFromRedisClient(c.raw)
		if err := inspector.DeleteTask(o.Queue, id); err != nil && !errors.Is(err, asynq.ErrTaskNotFound) && !errors.Is(err, asynq.ErrQueueNotFound) {
			t.Error(SafeError(err))
		}
	}()
	info, err := c.Enqueue(ctx, Submission{ID: id, Type: "fixture.integration", Payload: json.RawMessage(`{"test":"roundtrip"}`)})
	if err != nil {
		t.Fatal(err)
	}
	out := await(t, c, info.ID, StateCompleted)
	if string(out.Result) != `{"test":"roundtrip"}` {
		t.Fatal("stored result mismatch")
	}
}
