/**
 * @Author: LiuShuXin
 * @Description: 队列配置、提交输入与配置加载组合验证
 * @File: options_test.go
 * Software: Goland
 * @Date: 2026/10/15
 */
package taskqueue

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cauliflower-beep/easy-go-framework/config"
	"go.uber.org/zap"
)

func TestOptionsValidation(t *testing.T) {
	base := DefaultOptions()
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Options){"queue": func(o *Options) { o.Queue = "bad{queue}" }, "concurrency": func(o *Options) { o.Concurrency = 0 }, "timeout": func(o *Options) { o.TaskTimeout = time.Millisecond }, "retention": func(o *Options) { o.Retention = 0 }, "operation": func(o *Options) { o.OperationTimeout = 0 }, "shutdown": func(o *Options) { o.ShutdownTimeout = 0 }, "poll": func(o *Options) { o.PollInterval = 0 }, "delayed": func(o *Options) { o.DelayedCheckInterval = 0 }, "retry delay": func(o *Options) { o.RetryDelay = 0 }, "payload": func(o *Options) { o.MaxPayloadBytes = 0 }, "result": func(o *Options) { o.MaxResultBytes = 0 }, "redis": func(o *Options) { o.Redis.Host = "" }} {
		t.Run(name, func(t *testing.T) {
			o := base
			mutate(&o)
			if o.Validate() == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
	if _, err := NewClient(base, nil); err == nil {
		t.Fatal("nil logger accepted")
	}
	if _, err := NewWorker(base, nil); err == nil {
		t.Fatal("nil logger accepted")
	}
	base.Redis.TLSRootCert = "missing-private-file.pem"
	if _, err := NewClient(base, zap.NewNop()); err == nil || strings.Contains(err.Error(), "private-file") {
		t.Fatal("CA validation/privacy failed")
	}
}

func TestSubmissionValidation(t *testing.T) {
	c, _, _ := fixture(t)
	for name, in := range map[string]Submission{"type": {Type: "bad type"}, "ID": {ID: "bad", Type: "fixture.test"}, "JSON": {Type: "fixture.test", Payload: json.RawMessage(`{"secret":"private`)}, "size": {Type: "fixture.test", Payload: json.RawMessage(`"` + strings.Repeat("x", c.options.MaxPayloadBytes) + `"`)}, "retry": {Type: "fixture.test", MaxRetry: -1}, "timeout": {Type: "fixture.test", Timeout: -time.Second}, "delay": {Type: "fixture.test", Delay: time.Millisecond}} {
		t.Run(name, func(t *testing.T) {
			if _, err := c.Enqueue(context.Background(), in); err == nil {
				t.Fatal("invalid submission accepted")
			}
		})
	}
}

func TestValidateSubmissionMatchesEnqueueWithoutIO(t *testing.T) {
	c, _, r := fixture(t)
	in := Submission{Type: "fixture.validate", Payload: json.RawMessage(`{"value":1}`)}
	if err := c.ValidateSubmission(in); err != nil {
		t.Fatal(err)
	}
	if len(r.Keys()) != 0 {
		t.Fatal("validation wrote queue data")
	}
	// 空载荷会作为 JSON null 持久化，校验必须使用实际的四字节大小。
	c.options.MaxPayloadBytes = 1
	in.Payload = nil
	if c.ValidateSubmission(in) == nil {
		t.Fatal("validation ignored normalized null payload size")
	}
	if _, err := c.Enqueue(context.Background(), in); err == nil {
		t.Fatal("enqueue disagreed with validation")
	}
	if len(r.Keys()) != 0 {
		t.Fatal("invalid submission reached Redis")
	}
}

func TestConfigurationComposition(t *testing.T) {
	type settings struct {
		Tasks Options `mapstructure:"tasks"`
	}
	var cfg settings
	t.Setenv("QUEUE_FIXTURE_TASKS_QUEUE", "fixture-tasks")
	t.Setenv("QUEUE_FIXTURE_TASKS_REDIS_PASSWORD", "private-password")
	if err := config.Load("", &cfg, config.Options{EnvPrefix: "QUEUE_FIXTURE", Defaults: map[string]any{"tasks.concurrency": 3, "tasks.task_timeout": time.Minute}}); err != nil {
		t.Fatal(err)
	}
	if cfg.Tasks.Queue != "fixture-tasks" || cfg.Tasks.Concurrency != 3 || cfg.Tasks.TaskTimeout != time.Minute || cfg.Tasks.Redis.Password != "private-password" {
		t.Fatal("configuration composition failed")
	}
}
