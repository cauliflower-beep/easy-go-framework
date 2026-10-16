/**
 * @Author: LiuShuXin
 * @Description: 验证组件默认配置组合、覆盖优先级、独立解码与失败原子性
 * @File: defaults_test.go
 * Software: Goland
 * @Date: 2026/10/15
 */
package config_test

import (
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/cauliflower-beep/easy-go-framework/config"
	"github.com/cauliflower-beep/easy-go-framework/httpserver"
	"github.com/cauliflower-beep/easy-go-framework/logging"
	"github.com/cauliflower-beep/easy-go-framework/postgres"
)

type componentSettings struct {
	HTTP     httpserver.Options `mapstructure:"http"`
	Log      logging.Options    `mapstructure:"log"`
	Postgres postgres.Options   `mapstructure:"postgres"`
}

type defaultRule struct {
	Hosts  []string          `mapstructure:"hosts"`
	Labels map[string]string `mapstructure:"labels"`
}

type referenceSettings struct {
	Rules    []defaultRule          `mapstructure:"rules"`
	Named    map[string]defaultRule `mapstructure:"named"`
	Pointers []*defaultRule         `mapstructure:"pointers"`
	Dynamic  any                    `mapstructure:"dynamic"`
	Array    [1]defaultRule         `mapstructure:"array"`
	Nil      []string               `mapstructure:"nil"`
	Empty    []string               `mapstructure:"empty"`
}

func referenceDefaults() referenceSettings {
	rule := func() defaultRule {
		return defaultRule{Hosts: []string{"original"}, Labels: map[string]string{"name": "original"}}
	}
	pointed := rule()
	return referenceSettings{Rules: []defaultRule{rule()}, Named: map[string]defaultRule{"first": rule()}, Pointers: []*defaultRule{&pointed}, Dynamic: []defaultRule{rule()}, Array: [1]defaultRule{rule()}, Empty: []string{}}
}

func mutateReferenceSettings(value *referenceSettings) {
	value.Rules[0].Hosts[0] = "changed"
	value.Rules[0].Labels["name"] = "changed"
	value.Named["first"].Hosts[0] = "changed"
	value.Named["first"].Labels["name"] = "changed"
	value.Pointers[0].Hosts[0] = "changed"
	value.Dynamic.([]defaultRule)[0].Hosts[0] = "changed"
	value.Array[0].Hosts[0] = "changed"
}

func TestCompositeDefaultsDoNotShareReferences(t *testing.T) {
	for _, pointer := range []bool{false, true} {
		defaults := referenceDefaults()
		var source any = defaults
		if pointer {
			source = &defaults
		}
		var first, second referenceSettings
		for _, target := range []*referenceSettings{&first, &second} {
			if err := config.Load("", target, config.Options{DefaultsFrom: source}); err != nil {
				t.Fatal(err)
			}
		}
		if first.Nil != nil || first.Empty == nil {
			t.Fatal("nil and empty slices changed")
		}
		mutateReferenceSettings(&first)
		if !reflect.DeepEqual(defaults, referenceDefaults()) || !reflect.DeepEqual(second, referenceDefaults()) {
			t.Fatal("decoded composite references alias defaults or another load")
		}
	}
}

func TestExplicitDefaultsAlsoIsolateCompositeReferences(t *testing.T) {
	defaults := referenceDefaults()
	var got referenceSettings
	if err := config.Load("", &got, config.Options{Defaults: map[string]any{
		"rules": defaults.Rules, "named": defaults.Named, "pointers": defaults.Pointers,
		"dynamic": defaults.Dynamic, "array": defaults.Array, "empty": defaults.Empty,
	}}); err != nil {
		t.Fatal(err)
	}
	mutateReferenceSettings(&got)
	if !reflect.DeepEqual(defaults, referenceDefaults()) {
		t.Fatal("explicit defaults alias decoded values")
	}
}

func TestConcurrentDefaultLoadsAreIndependent(t *testing.T) {
	defaults := referenceDefaults()
	var group sync.WaitGroup
	for range 12 {
		group.Go(func() {
			var got referenceSettings
			if err := config.Load("", &got, config.Options{DefaultsFrom: defaults}); err != nil {
				t.Error(err)
				return
			}
			mutateReferenceSettings(&got)
		})
	}
	group.Wait()
	if !reflect.DeepEqual(defaults, referenceDefaults()) {
		t.Fatal("concurrent loads mutated shared defaults")
	}
}

func TestCyclicDefaultsFailWithoutChangingTarget(t *testing.T) {
	type settings struct {
		Data map[string]any `mapstructure:"data"`
	}
	cycle := map[string]any{}
	cycle["self"] = cycle
	for _, options := range []config.Options{
		{DefaultsFrom: settings{Data: cycle}},
		{Defaults: map[string]any{"data": cycle}},
	} {
		target := settings{Data: map[string]any{"unchanged": true}}
		if err := config.Load("", &target, options); err == nil {
			t.Fatal("cyclic defaults accepted")
		}
		if !reflect.DeepEqual(target.Data, map[string]any{"unchanged": true}) {
			t.Fatal("failed load modified target")
		}
	}
}

type skippedDefaultRule struct {
	Name    string `mapstructure:"name"`
	Ignored any    `mapstructure:"-"`
	private any
}

func TestSkippedDefaultReferencesFailAtomically(t *testing.T) {
	type settings struct {
		Data any `mapstructure:"data"`
	}
	pointed := "private-default-value"
	for _, reference := range []struct {
		name  string
		value any
	}{
		{"map", map[string]string{"value": pointed}},
		{"slice", []string{pointed}},
		{"pointer", &pointed},
		{"array", [1][]string{{pointed}}},
		{"struct", struct{ Values []string }{Values: []string{pointed}}},
		{"function", func() {}},
		{"channel", make(chan int)},
	} {
		for _, field := range []string{"Ignored", "private"} {
			t.Run(reference.name+"/"+field, func(t *testing.T) {
				rule := skippedDefaultRule{Name: "ordinary-data"}
				if field == "Ignored" {
					rule.Ignored = reference.value
				} else {
					rule.private = reference.value
				}
				for _, data := range []any{[]skippedDefaultRule{rule}, map[string]skippedDefaultRule{"first": rule}, &rule, [1]skippedDefaultRule{rule}} {
					for _, options := range []config.Options{
						{DefaultsFrom: settings{Data: data}},
						{Defaults: map[string]any{"data": data}},
					} {
						target := settings{Data: "unchanged"}
						err := config.Load("", &target, options)
						if err == nil || !strings.Contains(err.Error(), field) {
							t.Fatalf("unsafe skipped field accepted or not identified: %v", err)
						}
						if target.Data != "unchanged" {
							t.Fatal("rejected defaults modified target")
						}
						if strings.Contains(err.Error(), pointed) {
							t.Fatal("default value leaked into validation error")
						}
					}
				}
			})
		}
	}
}

func TestSkippedDefaultValuesRemainSupported(t *testing.T) {
	type settings struct {
		Rules []skippedDefaultRule `mapstructure:"rules"`
	}
	for _, value := range []any{
		nil, "ordinary-data", 3, [1]string{"ordinary-data"}, struct{ Enabled bool }{true},
		map[string]string(nil), []string(nil), (*int)(nil), (func())(nil), (chan int)(nil),
	} {
		defaults := settings{Rules: []skippedDefaultRule{{Name: "ordinary-data", Ignored: value, private: value}}}
		for _, options := range []config.Options{
			{DefaultsFrom: defaults},
			{Defaults: map[string]any{"rules": defaults.Rules}},
		} {
			var got settings
			if err := config.Load("", &got, options); err != nil {
				t.Fatalf("safe skipped value %T rejected: %v", value, err)
			}
			if !reflect.DeepEqual(got, defaults) {
				t.Fatalf("safe skipped value %T was cleared or changed", value)
			}
		}
	}
}

func TestComposedDefaultOptions(t *testing.T) {
	defaults := componentSettings{HTTP: httpserver.DefaultOptions(), Log: logging.DefaultOptions(), Postgres: postgres.DefaultOptions()}
	defaults.Postgres.Database, defaults.Postgres.User = "fixture", "fixture"
	for _, pointer := range []bool{false, true} {
		var source any = defaults
		if pointer {
			source = &defaults
		}
		var got componentSettings
		if err := config.Load("", &got, config.Options{DefaultsFrom: source}); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, defaults) {
			t.Fatal("typed defaults did not round-trip")
		}
		if err := got.HTTP.Validate(); err != nil {
			t.Fatal(err)
		}
		if err := got.Log.Validate(); err != nil {
			t.Fatal(err)
		}
		if err := got.Postgres.Validate(); err != nil {
			t.Fatal(err)
		}
		got.HTTP.CORS.AllowedMethods[0] = "OTHER"
		if defaults.HTTP.CORS.AllowedMethods[0] == "OTHER" {
			t.Fatal("decoded slice aliases default config")
		}
	}
}

func TestTypedDefaultPrecedenceAndInvalidSources(t *testing.T) {
	clearEnvironment(t)
	defaults := fixture{Name: "struct", Count: 3, Enabled: true, Labels: []string{"default"}}
	path := writeConfig(t, "name: file\ncount: 5\nenabled: false\nlabels: []\n")
	t.Setenv("TESTAPP_NAME", "environment")
	got := fixture{}
	if err := config.Load(path, &got, config.Options{EnvPrefix: "TESTAPP", DefaultsFrom: defaults, Defaults: map[string]any{"count": 4}}); err != nil {
		t.Fatal(err)
	}
	if got.Name != "environment" || got.Count != 5 || got.Enabled || len(got.Labels) != 0 {
		t.Fatalf("override priority or explicit zero lost: %+v", got)
	}
	if err := config.Load("", &got, config.Options{DefaultsFrom: defaults, Defaults: map[string]any{"count": 4}}); err != nil || got.Count != 4 {
		t.Fatalf("explicit default should override typed default: %v", err)
	}
	var missing *fixture
	for _, source := range []any{missing, 3, new(string), struct{}{}, &struct{}{}} {
		before := got
		if err := config.Load("", &got, config.Options{DefaultsFrom: source}); err == nil {
			t.Fatalf("invalid defaults accepted: %T", source)
		}
		if !reflect.DeepEqual(got, before) {
			t.Fatal("invalid default source modified target")
		}
	}
}
