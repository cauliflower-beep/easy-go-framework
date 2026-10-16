/**
 * @Author: LiuShuXin
 * @Description: 用独立测试结构验证公共配置机制、严格边界与加载隔离
 * @File: config_test.go
 * Software: Goland
 * @Date: 2026/10/10 18:00
 */

package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cauliflower-beep/easy-go-framework/config"
)

type fixture struct {
	Name    string        `mapstructure:"name"`
	Count   int           `mapstructure:"count"`
	Limit   int64         `mapstructure:"limit"`
	Enabled bool          `mapstructure:"enabled"`
	Delay   time.Duration `mapstructure:"delay"`
	Labels  []string      `mapstructure:"labels"`
	Remote  struct {
		Host string `mapstructure:"host"`
	} `mapstructure:"remote"`
}

func TestLoadMechanism(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		env        map[string]string
		wantErr    string
	}{
		{name: "defaults"}, {name: "empty mapping", yaml: "{}"},
		{name: "file", yaml: "name: file\n"},
		{name: "environment", yaml: "name: file\n", env: map[string]string{"TESTAPP_NAME": "env", "TESTAPP_COUNT": "4", "TESTAPP_LIMIT": "9", "TESTAPP_ENABLED": "true", "TESTAPP_DELAY": "2s", "TESTAPP_LABELS": "a, b", "TESTAPP_REMOTE_HOST": "local"}},
		{name: "unknown section", yaml: "unknown: {}", wantErr: "unknown"},
		{name: "unknown nested field", yaml: "remote:\n  typo: x", wantErr: "typo"},
		{name: "wrong case", yaml: "Name: x", wantErr: "Name"},
		{name: "float", yaml: "count: 2.0", wantErr: "count"},
		{name: "string integer", yaml: "count: '2'", wantErr: "count"},
		{name: "wrong string type", yaml: "name: 2", wantErr: "name"},
		{name: "wrong list type", yaml: "labels: [3]", wantErr: "labels"},
		{name: "wrong bool type", yaml: "enabled: 'true'", wantErr: "enabled"},
		{name: "duration requires unit", yaml: "delay: 2", wantErr: "delay"},
		{name: "null", yaml: "remote: null", wantErr: "null"},
		{name: "null sequence item", yaml: "labels: [null]", wantErr: "null"},
		{name: "malformed", yaml: "name: [", wantErr: "parse config"},
		{name: "duplicate", yaml: "name: a\nname: b", wantErr: "already defined"},
		{name: "multiple documents", yaml: "{}\n---\n{}", wantErr: "exactly one"},
		{name: "empty document", yaml: "# empty", wantErr: "parse config"},
		{name: "null root", yaml: "null", wantErr: "YAML mapping"},
		{name: "sequence root", yaml: "- a", wantErr: "parse config"},
		{name: "override cannot hide type error", yaml: "count: 2.5", env: map[string]string{"TESTAPP_COUNT": "4"}, wantErr: "count"},
		{name: "bad integer environment", env: map[string]string{"TESTAPP_COUNT": "bad"}, wantErr: "count"},
		{name: "bad bool environment", env: map[string]string{"TESTAPP_ENABLED": "bad"}, wantErr: "enabled"},
		{name: "bad duration environment", env: map[string]string{"TESTAPP_DELAY": "2"}, wantErr: "delay"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearEnvironment(t)
			for key, value := range tc.env {
				t.Setenv(key, value)
			}
			path := ""
			if tc.yaml != "" {
				path = writeConfig(t, tc.yaml)
			}
			got := fixture{Name: "untouched"}
			err := config.Load(path, &got, config.Options{EnvPrefix: "TESTAPP", Defaults: map[string]any{"name": "default", "count": 1}})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error=%v want=%s", err, tc.wantErr)
				}
				if !reflect.DeepEqual(got, fixture{Name: "untouched"}) {
					t.Fatalf("failed load modified target: %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := fixture{Name: "default", Count: 1}
			if tc.name == "file" {
				want.Name = "file"
			}
			if tc.name == "environment" {
				want = fixture{Name: "env", Count: 4, Limit: 9, Enabled: true, Delay: 2 * time.Second, Labels: []string{"a", "b"}}
				want.Remote.Host = "local"
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got=%+v want=%+v", got, want)
			}
		})
	}
}

func TestAbsentAndEmptyFieldsBelongToApplication(t *testing.T) {
	clearEnvironment(t)
	t.Setenv("TESTAPP_NAME", "")
	var got fixture
	if err := config.Load("", &got, config.Options{EnvPrefix: "TESTAPP", Defaults: map[string]any{"name": "default"}}); err != nil || got.Name != "" || got.Count != 0 {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	// 无前缀时完全禁用环境覆盖，缺省字段保留零值。
	if err := config.Load("", &got, config.Options{Defaults: map[string]any{"name": "default"}}); err != nil || got.Name != "default" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

func TestTargetsAndOptions(t *testing.T) {
	var missing *fixture
	for _, target := range []any{nil, missing, fixture{}, new(string)} {
		if err := config.Load("", target, config.Options{}); err == nil {
			t.Fatalf("invalid target accepted: %T", target)
		}
	}
	for _, options := range []config.Options{{EnvPrefix: "bad-prefix"}, {Defaults: map[string]any{"typo": 1}}} {
		if err := config.Load("", new(fixture), options); err == nil {
			t.Fatalf("invalid options accepted: %+v", options)
		}
	}
	var got fixture
	err := config.Load(filepath.Join(t.TempDir(), "missing.yaml"), &got, config.Options{})
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file error=%v", err)
	}
	t.Chdir(filepath.Dir(writeConfig(t, "invalid: [")))
	if err := config.Load("", &got, config.Options{}); err != nil {
		t.Fatalf("implicit discovery: %v", err)
	}
}

func TestIndependentAndConcurrentLoads(t *testing.T) {
	clearEnvironment(t)
	paths := []string{writeConfig(t, "name: first"), writeConfig(t, "name: second")}
	names := []string{"first", "second"}
	var workers sync.WaitGroup
	for i := range 16 {
		workers.Go(func() {
			var got fixture
			if err := config.Load(paths[i%2], &got, config.Options{EnvPrefix: "TESTAPP"}); err != nil || got.Name != names[i%2] {
				t.Errorf("got=%+v err=%v", got, err)
			}
		})
	}
	workers.Wait()
	var first, second fixture
	t.Setenv("TESTAPP_NAME", "first")
	if err := config.Load("", &first, config.Options{EnvPrefix: "TESTAPP"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TESTAPP_NAME", "second")
	if err := config.Load("", &second, config.Options{EnvPrefix: "TESTAPP"}); err != nil {
		t.Fatal(err)
	}
	if first.Name != "first" || second.Name != "second" {
		t.Fatal("loads share state")
	}
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func clearEnvironment(t *testing.T) {
	t.Helper()
	for _, key := range []string{"TESTAPP_NAME", "TESTAPP_COUNT", "TESTAPP_LIMIT", "TESTAPP_ENABLED", "TESTAPP_DELAY", "TESTAPP_LABELS", "TESTAPP_REMOTE_HOST"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
}
