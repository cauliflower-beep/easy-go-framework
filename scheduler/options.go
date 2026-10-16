/**
 * @Author: LiuShuXin
 * @Description: 定时调度配置、时区与计划的严格解析
 * @File: options.go
 * Software: Goland
 * @Date: 2026/10/15
 */

// Package scheduler 使用 cron 在当前进程定时调用处理函数，不依赖外部队列或 Redis。
package scheduler

import (
	"errors"
	"regexp"
	"strings"
	"time"
	_ "time/tzdata" // 交付独立 Windows/Linux 可执行文件时不依赖主机安装时区数据。

	"github.com/robfig/cron/v3"
)

type Options struct {
	TimeZone        string        `mapstructure:"time_zone"`
	RunTimeout      time.Duration `mapstructure:"run_timeout"`
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout"`
}

func DefaultOptions() Options {
	return Options{TimeZone: "UTC", RunTimeout: time.Minute, ShutdownTimeout: 5 * time.Second}
}
func (o Options) Validate() error {
	var errs []error
	if _, err := loadLocation(o.TimeZone); err != nil {
		errs = append(errs, err)
	}
	if o.RunTimeout <= 0 || o.ShutdownTimeout <= 0 {
		errs = append(errs, errors.New("scheduler run and shutdown timeouts must be positive"))
	}
	return errors.Join(errs...)
}

// Plan 是可组合进应用 YAML 的静态计划；处理函数由应用显式绑定。
// TimeZone 为空时继承 Options；Timeout=0 继承全局 RunTimeout。
type Plan struct {
	Name     string        `mapstructure:"name"`
	Spec     string        `mapstructure:"spec"`
	TimeZone string        `mapstructure:"time_zone"`
	Timeout  time.Duration `mapstructure:"timeout"`
}

var planNamePattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)
var parser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

func loadLocation(name string) (*time.Location, error) {
	// Local 会隐式依赖操作系统；只接受明确时区，统一 Windows 与 Linux 的语义。
	if name == "" || name == "Local" || len(name) > 128 || strings.TrimSpace(name) != name {
		return nil, errors.New("scheduler requires an explicit IANA time zone or UTC")
	}
	location, err := time.LoadLocation(name)
	if err != nil {
		return nil, errors.New("scheduler time zone is invalid")
	}
	return location, nil
}

func compile(p Plan, defaultZone string) (cron.Schedule, Plan, error) {
	if p.Timeout < 0 {
		return nil, p, errors.New("scheduler plan timeout must not be negative")
	}
	if !planNamePattern.MatchString(p.Name) {
		return nil, p, errors.New("scheduler plan name must start with a lowercase letter and contain 1-64 letters, digits, dots, underscores or hyphens")
	}
	if p.Spec == "" || len(p.Spec) > 256 || strings.TrimSpace(p.Spec) != p.Spec || strings.ContainsAny(p.Spec, "\r\n\x00") {
		return nil, p, errors.New("scheduler schedule is invalid")
	}
	if strings.HasPrefix(p.Spec, "TZ=") || strings.HasPrefix(p.Spec, "CRON_TZ=") {
		return nil, p, errors.New("scheduler time zone must be configured through time_zone")
	}
	if strings.HasPrefix(p.Spec, "@every") {
		fields := strings.Fields(p.Spec)
		if len(fields) != 2 {
			return nil, p, errors.New("scheduler interval requires @every followed by a duration")
		}
		duration, err := time.ParseDuration(fields[1])
		if err != nil || duration < time.Second || duration%time.Second != 0 {
			return nil, p, errors.New("scheduler interval must be positive whole seconds")
		}
	}
	if p.TimeZone == "" {
		p.TimeZone = defaultZone
	}
	location, err := loadLocation(p.TimeZone)
	if err != nil {
		return nil, p, err
	}
	schedule, err := parser.Parse(p.Spec)
	if err != nil {
		return nil, p, errors.New("scheduler requires a valid five-field cron expression, descriptor or @every interval")
	}
	if spec, ok := schedule.(*cron.SpecSchedule); ok {
		spec.Location = location
	}
	if schedule.Next(time.Now().In(location)).IsZero() {
		return nil, p, errors.New("scheduler schedule has no reachable next occurrence")
	}
	return schedule, p, nil
}
