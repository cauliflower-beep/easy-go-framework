/**
 * @Author: LiuShuXin
 * @Description: 在同一进程比较普通与池化 Reader 的严格 JSON 绑定开销
 * @File: reader_benchmark_test.go
 * Software: Goland
 * @Date: 2026/10/13
 */
package httpapi

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func BenchmarkStrictJSONReader(b *testing.B) {
	for _, size := range []int{32, 4096, 65536, 524288} {
		for _, strategy := range []struct {
			name string
			bind func(*http.Request, any) error
		}{
			{"new", func(request *http.Request, dst any) error {
				return decodeJSONObject(bufio.NewReader(request.Body), dst)
			}},
			{"pooled", strictJSONBinding{}.Bind},
		} {
			b.Run(fmt.Sprintf("body_%d/%s", size, strategy.name), func(b *testing.B) {
				body := `{"name":"` + strings.Repeat("x", size) + `","count":2}`
				reader := strings.NewReader(body)
				request := httptest.NewRequest("POST", "/fixture", nil)
				request.Body = io.NopCloser(reader)
				b.ReportAllocs()
				b.SetBytes(int64(len(body)))
				b.ResetTimer()
				for b.Loop() {
					reader.Reset(body)
					var input struct {
						Name  string `json:"name" binding:"required,min=2"`
						Count int    `json:"count" binding:"gte=0,lte=10"`
					}
					if err := strategy.bind(request, &input); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
