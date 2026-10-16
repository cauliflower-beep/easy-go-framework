/**
 * @Author: LiuShuXin
 * @Description: 生成带前缀的紧凑小写请求关联标识
 * @File: request_id.go
 * Software: Goland
 * @Date: 2026/10/13
 */
package httpserver

import (
	"crypto/rand"
	"encoding/base32"
)

var requestIDEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

func newRequestID() string {
	// 12 字节保留 96 位随机性；20 位 Base32 加 req_，共 24 个字符。
	// 标识只做请求关联，不承载路由、用户信息或认证语义。
	var data [12]byte
	rand.Read(data[:]) // 当前 Go 的 crypto/rand.Read 不返回错误，熵源失败会终止进程。
	return "req_" + requestIDEncoding.EncodeToString(data[:])
}
