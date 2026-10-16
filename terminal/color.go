/**
 * @Author: LiuShuXin
 * @Description: 统一终端检测与 ANSI 开关，供日志和启动展示复用
 * @File: color.go
 * Software: Goland
 * @Date: 2026/10/10 16:00
 */

// Package terminal 统一终端输出能力，不决定日志格式和部署环境策略。
package terminal

import (
	"io"
	"os"

	"github.com/mattn/go-isatty"
)

// IsTerminal 仅将真实终端或 MSYS/Cygwin 伪终端识别为交互输出。
func IsTerminal(output io.Writer) bool {
	file, ok := output.(*os.File)
	return ok && (isatty.IsTerminal(file.Fd()) || isatty.IsCygwinTerminal(file.Fd()))
}

// Color 返回 ANSI 可用状态与恢复函数；调用方在输出结束后恢复终端模式。
// always 允许向 IDE 管道输出颜色，但不会向普通文件写入颜色。
func Color(output io.Writer, mode string) (bool, func()) {
	unchanged := func() {}
	if mode == "never" || os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false, unchanged
	}
	file, isFile := output.(*os.File)
	if isFile {
		if info, err := file.Stat(); err != nil || info.Mode().IsRegular() {
			return false, unchanged
		}
	}
	if !IsTerminal(output) {
		return mode == "always", unchanged
	}
	return enableColor(file.Fd())
}
