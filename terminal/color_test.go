/**
 * @Author: LiuShuXin
 * @Description: 验证 IDE 强制颜色、重定向及环境禁用边界
 * @File: color_test.go
 * Software: Goland
 * @Date: 2026/10/10 16:00
 */

package terminal

import (
	"bytes"
	"os"
	"testing"
)

func TestColorPolicyForNonTerminal(t *testing.T) {
	for _, tc := range []struct {
		name, mode, noColor, term string
		want                      bool
	}{
		{"auto pipe", "auto", "", "xterm-256color", false},
		{"IDE override", "always", "", "xterm-256color", true},
		{"disabled", "never", "", "xterm-256color", false},
		{"NO_COLOR overrides always", "always", "1", "xterm-256color", false},
		{"dumb terminal", "always", "", "dumb", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("NO_COLOR", tc.noColor)
			t.Setenv("TERM", tc.term)
			var output bytes.Buffer
			got, restore := Color(&output, tc.mode)
			defer restore()
			if got != tc.want || IsTerminal(&output) {
				t.Fatalf("color=%t want=%t", got, tc.want)
			}
		})
	}
}

func TestRegularFilesNeverEnableColor(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	file, err := os.CreateTemp(t.TempDir(), "stdout-*")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"auto", "always"} {
		got, restore := Color(file, mode)
		restore()
		if got || IsTerminal(file) {
			t.Fatal("regular output file enabled ANSI")
		}
	}
	file.Close()
	got, restore := Color(file, "always")
	restore()
	if got {
		t.Fatal("closed output enabled ANSI")
	}
}
