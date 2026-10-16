/**
 * @Author: LiuShuXin
 * @Description: 时间表达式、跨时区、夏令时和 YAML 组合验证
 * @File: options_test.go
 * Software: Goland
 * @Date: 2026/10/15
 */
package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cauliflower-beep/easy-go-framework/config"
	"go.uber.org/zap"
)

func TestOptionsValidation(t *testing.T) {
	o := DefaultOptions()
	if err := o.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, zone := range []string{"", "Local", "invalid/private-zone", " UTC"} {
		bad := o
		bad.TimeZone = zone
		if bad.Validate() == nil {
			t.Fatal("invalid or implicit time zone accepted")
		}
	}
	bad := o
	bad.RunTimeout = 0
	if bad.Validate() == nil {
		t.Fatal("run timeout accepted")
	}
	bad = o
	bad.ShutdownTimeout = 0
	if bad.Validate() == nil {
		t.Fatal("shutdown timeout accepted")
	}
	s := fixture(t)
	if _, err := New(o, nil); err == nil {
		t.Fatal("nil logger accepted")
	}
	if err := s.Register(plan("nil-handler"), nil); err == nil {
		t.Fatal("nil handler accepted")
	}
	if err := s.Shutdown(nil); err == nil {
		t.Fatal("nil context accepted")
	}
}

func TestPlanValidation(t *testing.T) {
	s := fixture(t)
	for name, mutate := range map[string]func(*Plan){
		"name": func(p *Plan) { p.Name = "bad name" }, "spec": func(p *Plan) { p.Spec = "invalid" }, "seconds": func(p *Plan) { p.Spec = "0 0 3 * * *" }, "year": func(p *Plan) { p.Spec = "0 0 3 * * * 2026" }, "reboot": func(p *Plan) { p.Spec = "@reboot" }, "impossible": func(p *Plan) { p.Spec = "0 0 31 2 *" }, "inline zone": func(p *Plan) { p.Spec = "CRON_TZ=UTC 0 0 * * *" }, "zone": func(p *Plan) { p.TimeZone = "Local" }, "subsecond": func(p *Plan) { p.Spec = "@every 100ms" }, "fraction": func(p *Plan) { p.Spec = "@every 1.5s" }, "zero": func(p *Plan) { p.Spec = "@every 0s" }, "negative": func(p *Plan) { p.Spec = "@every -1s" }, "timeout": func(p *Plan) { p.Timeout = -time.Second },
	} {
		t.Run(name, func(t *testing.T) {
			p := plan("validation")
			mutate(&p)
			if s.Register(p, func(context.Context) error { return nil }) == nil {
				t.Fatal("invalid plan accepted")
			}
		})
	}
	for i, spec := range []string{"0 3 * * *", "*/10 * * * *", "0 9 * * MON", "@daily", "@hourly", "@every 10m"} {
		p := plan(string(rune('a' + i)))
		p.Spec = spec
		if err := s.Register(p, func(context.Context) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTimeZonesAndDST(t *testing.T) {
	for _, test := range []struct{ name, spec, zone, after, want string }{
		{"shanghai", "0 3 * * *", "Asia/Shanghai", "2026-10-15T18:00:00Z", "2026-10-15T19:00:00Z"},
		{"utc", "0 3 * * *", "UTC", "2026-10-15T18:00:00Z", "2026-10-16T03:00:00Z"},
		{"spring-gap", "30 2 * * *", "America/New_York", "2026-03-08T05:00:00Z", "2026-03-09T06:30:00Z"},
		{"fall-first", "30 1 * * *", "America/New_York", "2026-11-01T04:00:00Z", "2026-11-01T05:30:00Z"},
		{"fall-second", "30 1 * * *", "America/New_York", "2026-11-01T05:30:00Z", "2026-11-01T06:30:00Z"},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := plan("timezone")
			p.Spec = test.spec
			p.TimeZone = test.zone
			schedule, _, err := compile(p, "UTC")
			if err != nil {
				t.Fatal(err)
			}
			after, _ := time.Parse(time.RFC3339, test.after)
			want, _ := time.Parse(time.RFC3339, test.want)
			if got := schedule.Next(after); !got.Equal(want) {
				t.Fatalf("next run %v expected %v", got, want)
			}
		})
	}
}

func TestYAMLComposition(t *testing.T) {
	type configuration struct {
		Scheduler Options `mapstructure:"scheduler"`
		Plans     []Plan  `mapstructure:"plans"`
	}
	file := filepath.Join(t.TempDir(), "scheduler.yaml")
	data := []byte("scheduler:\n  time_zone: Asia/Shanghai\n  run_timeout: 2s\n  shutdown_timeout: 3s\nplans:\n  - name: sample\n    spec: '0 3 * * *'\n    timeout: 2m\n")
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	var cfg configuration
	t.Setenv("SCHEDULER_FIXTURE_SCHEDULER_RUN_TIMEOUT", "4s")
	if err := config.Load(file, &cfg, config.Options{EnvPrefix: "SCHEDULER_FIXTURE"}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Scheduler.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Scheduler.RunTimeout != 4*time.Second || len(cfg.Plans) != 1 || cfg.Plans[0].Timeout != 2*time.Minute {
		t.Fatal("configuration decoding failed")
	}
	s, err := New(cfg.Scheduler, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Register(cfg.Plans[0], func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	entry, err := s.Get("sample")
	if err != nil || entry.TimeZone != "Asia/Shanghai" {
		t.Fatal("default time zone not inherited")
	}
}
