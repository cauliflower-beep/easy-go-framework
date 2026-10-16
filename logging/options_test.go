/**
 * @Author: LiuShuXin
 * @Description: 验证日志组件自身的元数据、输出与保留约束
 * @File: options_test.go
 * Software: Goland
 * @Date: 2026/10/10 18:00
 */

package logging

import (
	"strings"
	"testing"
)

func TestOptionsValidation(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		change     func(*Options)
	}{
		{"level", "log.level", func(o *Options) { o.Level = "fatal" }},
		{"format", "log.format", func(o *Options) { o.Format = "xml" }},
		{"color", "log.color", func(o *Options) { o.Color = "yes" }},
		{"output", "log.output", func(o *Options) { o.Output = "unknown" }},
		{"directory", "log.directory", func(o *Options) { o.Directory = " " }},
		{"local zone", "log.time_zone", func(o *Options) { o.TimeZone = "Local" }},
		{"unknown zone", "log.time_zone", func(o *Options) { o.TimeZone = "Unknown/Zone" }},
		{"zero retention", "log.retention_days", func(o *Options) { o.RetentionDays = 0 }},
		{"large retention", "log.retention_days", func(o *Options) { o.RetentionDays = 36501 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options := Options{Level: "info", Format: "auto", Color: "auto", Output: "console", Directory: "logs", TimeZone: "UTC", RetentionDays: 7}
			tc.change(&options)
			if err := options.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate=%v want=%s", err, tc.want)
			}
		})
	}
	if err := (Identity{Name: "other-service", Environment: "production"}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (Identity{}).Validate(); err == nil || !strings.Contains(err.Error(), "app.name") || !strings.Contains(err.Error(), "app.environment") {
		t.Fatalf("missing identity validation: %v", err)
	}
}
