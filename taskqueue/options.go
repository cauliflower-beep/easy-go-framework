/**
 * @Author: LiuShuXin
 * @Description: 持久化任务队列的独立连接与执行配置
 * @File: options.go
 * Software: Goland
 * @Date: 2026/10/15
 */

// Package taskqueue 基于 Asynq 提供持久化任务提交、后台执行和状态查询。
package taskqueue

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/cauliflower-beep/easy-go-framework/redis"
)

// Options 由应用配置，不自动启用 Worker，也不读取业务配置或环境变量。
// Redis 复用连接配置，但使用独立连接池，不占用普通缓存客户端。
type Options struct {
	Redis                redis.Options `mapstructure:"redis"`
	Queue                string        `mapstructure:"queue"`
	Concurrency          int           `mapstructure:"concurrency"`
	TaskTimeout          time.Duration `mapstructure:"task_timeout"`
	Retention            time.Duration `mapstructure:"retention"`
	OperationTimeout     time.Duration `mapstructure:"operation_timeout"`
	ShutdownTimeout      time.Duration `mapstructure:"shutdown_timeout"`
	PollInterval         time.Duration `mapstructure:"poll_interval"`
	DelayedCheckInterval time.Duration `mapstructure:"delayed_check_interval"`
	RetryDelay           time.Duration `mapstructure:"retry_delay"`
	MaxPayloadBytes      int           `mapstructure:"max_payload_bytes"`
	MaxResultBytes       int           `mapstructure:"max_result_bytes"`
}

func DefaultOptions() Options {
	return Options{Redis: redis.DefaultOptions(), Queue: "tasks", Concurrency: 4, TaskTimeout: 5 * time.Minute, Retention: 24 * time.Hour, OperationTimeout: 5 * time.Second, ShutdownTimeout: 10 * time.Second, PollInterval: time.Second, DelayedCheckInterval: time.Second, RetryDelay: 30 * time.Second, MaxPayloadBytes: 64 << 10, MaxResultBytes: 64 << 10}
}

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9._:-]{0,127}$`)
var queuePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

func (o Options) Validate() error {
	var errs []error
	errs = append(errs, o.Redis.Validate())
	if !queuePattern.MatchString(o.Queue) {
		errs = append(errs, errors.New("taskqueue.queue must be 1-64 lowercase letters, digits, underscores or hyphens, starting with a letter"))
	}
	if o.Concurrency <= 0 {
		errs = append(errs, errors.New("taskqueue.concurrency must be positive"))
	}
	for _, field := range []struct {
		name  string
		value time.Duration
	}{{"task_timeout", o.TaskTimeout}, {"retention", o.Retention}, {"retry_delay", o.RetryDelay}} {
		if field.value < time.Second || field.value%time.Second != 0 {
			errs = append(errs, fmt.Errorf("taskqueue.%s must be positive whole seconds", field.name))
		}
	}
	for _, field := range []struct {
		name  string
		value time.Duration
	}{{"operation_timeout", o.OperationTimeout}, {"shutdown_timeout", o.ShutdownTimeout}, {"poll_interval", o.PollInterval}, {"delayed_check_interval", o.DelayedCheckInterval}} {
		if field.value <= 0 {
			errs = append(errs, fmt.Errorf("taskqueue.%s must be positive", field.name))
		}
	}
	if o.MaxPayloadBytes <= 0 || o.MaxResultBytes <= 0 {
		errs = append(errs, errors.New("taskqueue payload and result limits must be positive"))
	}
	return errors.Join(errs...)
}
func (o Options) String() string {
	return fmt.Sprintf("taskqueue.Options{Queue:%q Concurrency:%d Redis:%s}", o.Queue, o.Concurrency, o.Redis.String())
}
func (o Options) GoString() string { return o.String() }
