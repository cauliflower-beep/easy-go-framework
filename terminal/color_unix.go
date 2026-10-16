//go:build !windows

/**
 * @Author: LiuShuXin
 * @Description: 探测非 Windows 终端的 ANSI 能力
 * @File: color_unix.go
 * Software: Goland
 * @Date: 2026/10/10 16:00
 */

package terminal

import "os"

func enableColor(uintptr) (bool, func()) {
	return os.Getenv("TERM") != "", func() {}
}
