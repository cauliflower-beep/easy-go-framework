//go:build windows

/**
 * @Author: LiuShuXin
 * @Description: 启用 Windows 终端 ANSI 并保存原始控制台模式
 * @File: color_windows.go
 * Software: Goland
 * @Date: 2026/10/10 16:00
 */

package terminal

import (
	"github.com/mattn/go-isatty"
	"golang.org/x/sys/windows"
)

func enableColor(fd uintptr) (bool, func()) {
	unchanged := func() {}
	handle := windows.Handle(fd)
	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err != nil {
		return isatty.IsCygwinTerminal(fd), unchanged
	}
	if mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING != 0 {
		return true, unchanged
	}
	// 不支持 ANSI 的旧控制台自动降级，避免显示转义字符。
	if err := windows.SetConsoleMode(handle, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING); err != nil {
		return false, unchanged
	}
	return true, func() { _ = windows.SetConsoleMode(handle, mode) }
}
