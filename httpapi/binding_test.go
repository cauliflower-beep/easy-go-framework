/**
 * @Author: LiuShuXin
 * @Description: 验证严格 JSON、字段路径、查询及路径参数和错误隐私
 * @File: binding_test.go
 * Software: Goland
 * @Date: 2026/10/13
 */
package httpapi_test

import (
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/cauliflower-beep/easy-go-framework/apperror"
	"github.com/cauliflower-beep/easy-go-framework/httpapi"
)

type fixtureInput struct {
	Name  string        `json:"name" binding:"required,min=2"`
	Count int           `json:"count" binding:"gte=0,lte=10"`
	Items []fixtureItem `json:"items" binding:"omitempty,dive"`
}
type fixtureItem struct {
	Label string `json:"label" binding:"required"`
}

func TestJSONBindingContract(t *testing.T) {
	for _, tc := range []struct {
		name, body, contentType, encoding string
		status                            int
		code                              apperror.Code
		field                             string
	}{
		{"valid", "{\"name\":\"ok\",\"count\":2}", "application/json", "", 200, apperror.CodeOK, ""},
		{"whitespace", " \n{\"name\":\"ok\"}\t", "application/json; charset=UTF-8", "identity", 200, apperror.CodeOK, ""},
		{"long surrounding whitespace", strings.Repeat(" \n\t", 2048) + `{"name":"ok","count":2}` + strings.Repeat(" \r\n", 2048), "application/json", "", 200, apperror.CodeOK, ""},
		{"large object", `{"name":"` + strings.Repeat("x", 65536) + `","count":2}`, "application/json", "", 200, apperror.CodeOK, ""},
		{"large object with second value", `{"name":"` + strings.Repeat("x", 65536) + `"} {}`, "application/json", "", 400, apperror.CodeInvalidJSON, ""},
		{"empty", "", "application/json", "", 400, apperror.CodeInvalidJSON, ""},
		{"only whitespace", " \n\t", "application/json", "", 400, apperror.CodeInvalidJSON, ""},
		{"null", "null", "application/json", "", 400, apperror.CodeInvalidJSON, ""},
		{"array", "[]", "application/json", "", 400, apperror.CodeInvalidJSON, ""},
		{"scalar", "42", "application/json", "", 400, apperror.CodeInvalidJSON, ""},
		{"broken", "{\"name\":", "application/json", "", 400, apperror.CodeInvalidJSON, ""},
		{"unknown", "{\"name\":\"ok\",\"private-field\":1}", "application/json", "", 400, apperror.CodeInvalidJSON, ""},
		{"unknown nested", "{\"name\":\"ok\",\"items\":[{\"label\":\"ok\",\"private-field\":1}]}", "application/json", "", 400, apperror.CodeInvalidJSON, ""},
		{"bad type", "{\"name\":\"ok\",\"count\":\"private-value\"}", "application/json", "", 400, apperror.CodeInvalidJSON, ""},
		{"second object", "{\"name\":\"ok\"} {}", "application/json", "", 400, apperror.CodeInvalidJSON, ""},
		{"second null", "{\"name\":\"ok\"} null", "application/json", "", 400, apperror.CodeInvalidJSON, ""},
		{"trailing invalid", "{\"name\":\"ok\"} private-trailing", "application/json", "", 400, apperror.CodeInvalidJSON, ""},
		{"required", "{}", "application/json", "", 400, apperror.CodeInvalidArgument, "name"},
		{"range", "{\"name\":\"ok\",\"count\":11}", "application/json", "", 400, apperror.CodeInvalidArgument, "count"},
		{"nested required", "{\"name\":\"ok\",\"items\":[{}]}", "application/json", "", 400, apperror.CodeInvalidArgument, "items[].label"},
		{"missing type", "{\"name\":\"ok\"}", "", "", 415, apperror.CodeUnsupportedMediaType, ""},
		{"text type", "{\"name\":\"ok\"}", "text/plain", "", 415, apperror.CodeUnsupportedMediaType, ""},
		{"bad charset", "{\"name\":\"ok\"}", "application/json; charset=gbk", "", 415, apperror.CodeUnsupportedMediaType, ""},
		{"compressed", "{\"name\":\"ok\"}", "application/json", "gzip", 415, apperror.CodeUnsupportedMediaType, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router, logs := testEngine()
			router.POST("/fixture", httpapi.Handle(func(c *gin.Context) error {
				var input fixtureInput
				if err := httpapi.BindJSON(c, &input); err != nil {
					return err
				}
				return httpapi.Success(c, map[string]int{"count": input.Count})
			}))
			request := httptest.NewRequest("POST", "/fixture", strings.NewReader(tc.body))
			request.Header.Set("Content-Type", tc.contentType)
			request.Header.Set("Content-Encoding", tc.encoding)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, request)
			response := decodeResponse(t, w)
			if w.Code != tc.status || response.Code != tc.code || strings.Contains(w.Body.String(), "private") || logs.Len() != 0 {
				t.Fatalf("unexpected result %d %s logs=%v", w.Code, w.Body, logs.All())
			}
			if tc.field != "" && (len(response.Details) != 1 || response.Details[0].Field != tc.field || response.Details[0].Rule == "") {
				t.Fatalf("invalid details: %+v", response.Details)
			}
		})
	}
}

func TestQueryAndURIBinding(t *testing.T) {
	router, _ := testEngine()
	router.GET("/query", httpapi.Handle(func(c *gin.Context) error {
		var input struct {
			Limit int `form:"limit" binding:"required,gte=1,lte=100"`
		}
		if err := httpapi.BindQuery(c, &input); err != nil {
			return err
		}
		return httpapi.Success(c, input.Limit)
	}))
	router.GET("/path/:id", httpapi.Handle(func(c *gin.Context) error {
		var input struct {
			ID int `uri:"id" binding:"required,gte=1"`
		}
		if err := httpapi.BindURI(c, &input); err != nil {
			return err
		}
		return httpapi.Success(c, input.ID)
	}))
	for _, tc := range []struct {
		path   string
		status int
		code   apperror.Code
		field  string
	}{
		{"/query?limit=10", 200, apperror.CodeOK, ""}, {"/query", 400, apperror.CodeInvalidArgument, "limit"}, {"/query?limit=101", 400, apperror.CodeInvalidArgument, "limit"},
		{"/query?limit=private-input", 400, apperror.CodeInvalidQuery, ""}, {"/query?limit=10&bad=%zz", 400, apperror.CodeInvalidQuery, ""},
		{"/path/2", 200, apperror.CodeOK, ""}, {"/path/0", 400, apperror.CodeInvalidArgument, "id"}, {"/path/private-input", 400, apperror.CodeInvalidPath, ""},
	} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", tc.path, nil))
		response := decodeResponse(t, w)
		if w.Code != tc.status || response.Code != tc.code || strings.Contains(w.Body.String(), "private") {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body)
		}
		if tc.field != "" && (len(response.Details) != 1 || response.Details[0].Field != tc.field) {
			t.Fatalf("%s details=%+v", tc.path, response.Details)
		}
	}
}

func TestDestinationMistakesAreInternalFailures(t *testing.T) {
	var missing *fixtureInput
	for _, destination := range []any{nil, missing, fixtureInput{}, new(int), new([]fixtureInput)} {
		for _, bind := range []func(*gin.Context, any) error{httpapi.BindJSON, httpapi.BindQuery, httpapi.BindURI} {
			router, logs := testEngine()
			router.POST("/fixture", httpapi.Handle(func(c *gin.Context) error { return bind(c, destination) }))
			w := httptest.NewRecorder()
			request := httptest.NewRequest("POST", "/fixture", strings.NewReader("{}"))
			request.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(w, request)
			if w.Code != 500 || decodeResponse(t, w).Code != apperror.CodeInternalError || logs.Len() != 1 {
				t.Fatal("programming mistake classified as bad request")
			}
		}
	}
}

func TestNestedMapPrivacyAndDetailLimit(t *testing.T) {
	router, _ := testEngine()
	router.POST("/fixture", httpapi.Handle(func(c *gin.Context) error {
		var input struct {
			Entries map[string]fixtureItem `json:"entries" binding:"dive"`
		}
		return httpapi.BindJSON(c, &input)
	}))
	var body strings.Builder
	body.WriteString(`{"entries":{`)
	for i := range 30 {
		if i > 0 {
			body.WriteByte(',')
		}
		fmt.Fprintf(&body, `"private-key-%d":{}`, i)
	}
	body.WriteString("}}")
	request := httptest.NewRequest("POST", "/fixture", strings.NewReader(body.String()))
	request.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, request)
	response := decodeResponse(t, w)
	if w.Code != 400 || len(response.Details) != 20 || strings.Contains(w.Body.String(), "private-key") {
		t.Fatalf("unsafe or unbounded validation: %d %s", w.Code, w.Body)
	}
	for _, detail := range response.Details {
		if detail.Field != "entries[].label" || detail.Rule != "required" {
			t.Fatalf("unexpected map detail: %+v", detail)
		}
	}
}

func TestConcurrentJSONBindingAndResponseIsolation(t *testing.T) {
	router, logs := testEngine()
	router.POST("/fixture", httpapi.Handle(func(c *gin.Context) error {
		var input fixtureInput
		if err := httpapi.BindJSON(c, &input); err != nil {
			return err
		}
		return httpapi.Success(c, input.Count)
	}))
	var workers sync.WaitGroup
	for i := range 48 {
		workers.Go(func() {
			body, status, code := `{"name":"ok","count":2}`, 200, apperror.CodeOK
			if i%2 == 0 {
				body, status, code = `{"name":"ok","count":11}`, 400, apperror.CodeInvalidArgument
			}
			request := httptest.NewRequest("POST", "/fixture", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, request)
			response := decodeResponse(t, w)
			if w.Code != status || response.Code != code {
				t.Errorf("response crossed requests: %d %s", w.Code, w.Body)
			}
			if status == 200 && response.Data != float64(2) {
				t.Errorf("unexpected data: %v", response.Data)
			}
		})
	}
	workers.Wait()
	if logs.Len() != 0 {
		t.Fatal("expected validation produced internal error logs")
	}
}

func BenchmarkJSONBindingAndResponse(b *testing.B) {
	router, _ := testEngine()
	router.POST("/fixture", httpapi.Handle(func(c *gin.Context) error {
		var input fixtureInput
		if err := httpapi.BindJSON(c, &input); err != nil {
			return err
		}
		return httpapi.Success(c, input.Count)
	}))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		request := httptest.NewRequest("POST", "/fixture", strings.NewReader(`{"name":"ok","count":2}`))
		request.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, request)
		if w.Code != 200 {
			b.Fatalf("status=%d", w.Code)
		}
	}
}

type failedBodyReader struct{}

func (failedBodyReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

type panicJSONValue struct{}

func (*panicJSONValue) UnmarshalJSON([]byte) error { panic("fixture decoder panic") }

func TestJSONBindingIsolationAfterFailuresAndPanic(t *testing.T) {
	router, _ := testEngine()
	router.POST("/fixture", httpapi.Handle(func(c *gin.Context) error {
		var input fixtureInput
		if err := httpapi.BindJSON(c, &input); err != nil {
			return err
		}
		return httpapi.Success(c, input)
	}))
	router.POST("/panic", httpapi.Handle(func(c *gin.Context) error {
		var input struct {
			Value panicJSONValue `json:"value"`
		}
		return httpapi.BindJSON(c, &input)
	}))
	for _, source := range []io.Reader{
		strings.NewReader(`{"name":"private-prefix`),
		io.MultiReader(strings.NewReader(`{"name":"private-prefix`), failedBodyReader{}),
		strings.NewReader(`{"name":"ok","unexpected":"private-value"}`),
	} {
		request := httptest.NewRequest("POST", "/fixture", source)
		request.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, request)
		if w.Code != 400 || strings.Contains(w.Body.String(), "private") {
			t.Fatal("failed input was accepted or exposed")
		}
	}
	func() {
		defer func() {
			if recover() != "fixture decoder panic" {
				t.Error("fixture panic did not propagate")
			}
		}()
		request := httptest.NewRequest("POST", "/panic", strings.NewReader(`{"value":{}}`))
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(httptest.NewRecorder(), request)
	}()
	request := httptest.NewRequest("POST", "/fixture", strings.NewReader(`{"name":"clean-input","count":7}`))
	request.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, request)
	response := decodeResponse(t, w)
	data, ok := response.Data.(map[string]any)
	if w.Code != 200 || !ok || data["name"] != "clean-input" || data["count"] != float64(7) {
		t.Fatalf("request state crossed failures: %d %s", w.Code, w.Body)
	}
}

// 复用请求夹具，仅计量绑定过程；覆盖小请求和跨缓冲区的大请求。
func BenchmarkJSONBinding(b *testing.B) {
	gin.SetMode(gin.TestMode)
	for _, size := range []int{32, 4096, 65536, 524288} {
		b.Run(fmt.Sprintf("body_%d", size), func(b *testing.B) {
			body := `{"name":"` + strings.Repeat("x", size) + `","count":2}`
			reader := strings.NewReader(body)
			request := httptest.NewRequest("POST", "/fixture", nil)
			request.Header.Set("Content-Type", "application/json")
			request.Body = io.NopCloser(reader)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = request
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			b.ResetTimer()
			for b.Loop() {
				reader.Reset(body)
				var input fixtureInput
				if err := httpapi.BindJSON(c, &input); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
