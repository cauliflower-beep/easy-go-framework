/**
 * @Author: LiuShuXin
 * @Description: 不依赖云 SDK 的对象操作输入输出与签名结果
 * @File: types.go
 * Software: Goland
 * @Date: 2026/10/15
 */
package objectstore

import (
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"
)

// PutInput 从 Body 当前游标上传至末尾；Body 归调用方所有，上传期间不得并发使用。
// 首版使用单次 PUT，最多 5 GiB；不缓存整个文件、不隐式创建 bucket 或分片上传。
type PutInput struct {
	Key         string
	Body        io.ReadSeeker
	ContentType string
	Metadata    map[string]string
}

// ObjectInfo 不把 ETag 解释为 MD5；不同存储的 ETag 算法不同。
type ObjectInfo struct {
	Size         int64
	ContentType  string
	ETag         string
	LastModified time.Time
	Metadata     map[string]string
}

// Object 必须读取到 EOF 或关闭 Body；Context/总期限一直覆盖正文读取。
type Object struct {
	ObjectInfo
	Body io.ReadCloser
}

// SignedRequest 是敏感的临时访问凭证；调用方必须发送 Headers，禁止修改 URL。
// JSON 输出供有意交付给客户端使用，不能当作日志数据。
type SignedRequest struct {
	URL       string      `json:"url"`
	Method    string      `json:"method"`
	Headers   http.Header `json:"headers"`
	ExpiresAt time.Time   `json:"expires_at"`
}

func (s SignedRequest) String() string   { return "objectstore.SignedRequest{[REDACTED]}" }
func (s SignedRequest) GoString() string { return s.String() }

func validKey(key string) error {
	if key == "" || len(key) > 1024 || !utf8.ValidString(key) || strings.HasPrefix(key, "/") || strings.HasPrefix(key, "\\") || strings.ContainsAny(key, "\x00\r\n") {
		return errors.New("objectstore key must be valid UTF-8, 1-1024 bytes, without leading slash or control delimiters")
	}
	return nil
}

func validatePut(in PutInput) (int64, error) {
	if err := validKey(in.Key); err != nil {
		return 0, err
	}
	if in.Body == nil || isNilReader(in.Body) {
		return 0, errors.New("objectstore upload body is required")
	}
	if strings.ContainsAny(in.ContentType, "\r\n\x00") {
		return 0, errors.New("objectstore content type is invalid")
	}
	total := 0
	for k, v := range in.Metadata {
		if k == "" || strings.Trim(k, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" || strings.ContainsAny(v, "\r\n\x00") {
			return 0, errors.New("objectstore metadata requires lowercase ASCII names and values without control delimiters")
		}
		total += len(k) + len(v)
	}
	if total > 2048 {
		return 0, errors.New("objectstore metadata must not exceed 2048 bytes")
	}
	start, err := in.Body.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, SafeError(err)
	}
	end, err := in.Body.Seek(0, io.SeekEnd)
	_, restore := in.Body.Seek(start, io.SeekStart)
	if err != nil || restore != nil {
		return 0, SafeError(errors.Join(err, restore))
	}
	if start < 0 || end < start || end-start > 5<<30 {
		return 0, errors.New("objectstore single PUT requires 0-5 GiB remaining data")
	}
	return end - start, nil
}

func isNilReader(v io.ReadSeeker) bool {
	r := reflect.ValueOf(v)
	switch r.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return r.IsNil()
	}
	return false
}

func cloneMetadata(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	result := make(map[string]string, len(m))
	for k, v := range m {
		result[k] = v
	}
	return result
}
