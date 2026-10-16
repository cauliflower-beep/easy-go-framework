/**
 * @Author: LiuShuXin
 * @Description: 校验 JSON、查询和路径参数，返回不包含输入值的错误
 * @File: binding.go
 * Software: Goland
 * @Date: 2026/10/13
 */
package httpapi

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
	"github.com/go-playground/validator/v10"

	"github.com/cauliflower-beep/easy-go-framework/apperror"
	"github.com/cauliflower-beep/easy-go-framework/logging"
)

type validationError struct {
	cause   error
	details []Violation
}

func (e *validationError) Error() string    { return "invalid request parameters" }
func (e *validationError) Unwrap() error    { return e.cause }
func (e *validationError) GoString() string { return e.Error() }

// 原始解码和校验错误可能包含输入值；字段与规则由公开 Details 单独提供。
func (e *validationError) LogDiagnostic() logging.Diagnostic {
	return logging.Diagnostic{Message: e.Error()}
}

type mediaTypeError struct{}

func (*mediaTypeError) Error() string { return "unsupported request media type" }

// BindJSON 接收单个 JSON 对象，配合 httpserver 的 BodyLimit 使用。
// 使用局部解码器，不修改 Gin 全局开关；校验复用 Gin 的 binding 标签。
func BindJSON(c *gin.Context, dst any) error {
	if err := checkDestination(dst); err != nil {
		return err
	}
	media, params, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	encoding := c.GetHeader("Content-Encoding")
	if err != nil || media != "application/json" || (params["charset"] != "" && !strings.EqualFold(params["charset"], "utf-8")) || (encoding != "" && !strings.EqualFold(encoding, "identity")) {
		return &mediaTypeError{}
	}
	if c.Request.Body == nil {
		return invalidBinding(apperror.CodeInvalidJSON, errors.New("missing JSON body"), dst, "json")
	}
	return normalizeBinding(c.ShouldBindWith(dst, strictJSONBinding{}), dst, "json", apperror.CodeInvalidJSON)
}

func BindQuery(c *gin.Context, dst any) error {
	if err := checkDestination(dst); err != nil {
		return err
	}
	// URL.Query 会静默丢弃有问题的键值，先显式检查编码错误。
	if _, err := url.ParseQuery(c.Request.URL.RawQuery); err != nil {
		return invalidBinding(apperror.CodeInvalidQuery, err, dst, "form")
	}
	return normalizeBinding(c.ShouldBindQuery(dst), dst, "form", apperror.CodeInvalidQuery)
}

func BindURI(c *gin.Context, dst any) error {
	if err := checkDestination(dst); err != nil {
		return err
	}
	return normalizeBinding(c.ShouldBindUri(dst), dst, "uri", apperror.CodeInvalidPath)
}

func checkDestination(dst any) error {
	v := reflect.ValueOf(dst)
	if !v.IsValid() || v.Kind() != reflect.Pointer || v.IsNil() || v.Elem().Kind() != reflect.Struct {
		return errors.New("binding destination must be a non-nil struct pointer")
	}
	return nil
}

type strictJSONBinding struct{}

var jsonReaderPool = sync.Pool{New: func() any { return bufio.NewReader(nil) }}

func (strictJSONBinding) Name() string { return "json" }
func (strictJSONBinding) Bind(request *http.Request, dst any) error {
	reader := jsonReaderPool.Get().(*bufio.Reader)
	reader.Reset(request.Body)
	// 覆盖解码、校验失败及 panic 路径；不让空闲 Reader 持有请求体引用。
	defer func() {
		reader.Reset(nil)
		jsonReaderPool.Put(reader)
	}()
	return decodeJSONObject(reader, dst)
}

// Reader 生命周期由调用方管理，严格解码和校验不持有请求资源。
func decodeJSONObject(reader *bufio.Reader, dst any) error {
	for {
		first, err := reader.ReadByte()
		if err != nil {
			return err
		}
		if first == ' ' || first == '\t' || first == '\r' || first == '\n' {
			continue
		}
		if first != '{' {
			return errors.New("JSON body must be an object")
		}
		if err := reader.UnreadByte(); err != nil {
			return err
		}
		break
	}
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return err
		}
		return errors.New("multiple JSON values are not allowed")
	}
	return binding.Validator.ValidateStruct(dst)
}

func normalizeBinding(err error, dst any, tag string, code apperror.Code) error {
	if err == nil {
		return nil
	}
	// 错误 DTO 或错误校验器配置属于程序错误，不伪装成客户端参数错误。
	var invalid *validator.InvalidValidationError
	if errors.As(err, &invalid) {
		return err
	}
	return invalidBinding(code, err, dst, tag)
}

func invalidBinding(code apperror.Code, cause error, dst any, tag string) error {
	var validations validator.ValidationErrors
	var details []Violation
	message := "invalid request parameters"
	switch code {
	case apperror.CodeInvalidJSON:
		message = "invalid JSON body"
	case apperror.CodeInvalidQuery:
		message = "invalid query parameters"
	case apperror.CodeInvalidPath:
		message = "invalid path parameters"
	}
	var mismatch *json.UnmarshalTypeError
	if errors.As(cause, &mismatch) && mismatch.Field != "" {
		details = append(details, Violation{Field: mismatch.Field, Rule: "type"})
	}
	if errors.As(cause, &validations) {
		code = apperror.CodeInvalidArgument
		// 限制嵌套列表可能产生的错误数量，不返回输入值及校验参数。
		for _, field := range validations {
			if len(details) >= 20 {
				break
			}
			details = append(details, Violation{Field: resolvePublicFieldPath(reflect.TypeOf(dst).Elem(), field.StructNamespace(), tag), Rule: field.Tag()})
		}
	}
	return &validationError{cause: apperror.Wrap(apperror.InvalidArgument, code, message, cause), details: details}
}

// 根据 DTO 标签生成外部字段路径；集合索引统一为 []，避免暴露动态 map 键。
func resolvePublicFieldPath(root reflect.Type, namespace, tag string) string {
	parts := strings.Split(namespace, ".")
	if root.Name() != "" && len(parts) > 0 && parts[0] == root.Name() {
		parts = parts[1:]
	}
	var path []string
	t := root
	for _, part := range parts {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct {
			return "request"
		}
		name := strings.SplitN(part, "[", 2)[0]
		field, ok := t.FieldByName(name)
		if !ok {
			return "request"
		}
		external := strings.SplitN(field.Tag.Get(tag), ",", 2)[0]
		if external == "-" {
			return "request"
		}
		if external == "" && !field.Anonymous {
			external = field.Name
		}
		t = field.Type
		for range strings.Count(part, "[") {
			for t.Kind() == reflect.Pointer {
				t = t.Elem()
			}
			if t.Kind() != reflect.Slice && t.Kind() != reflect.Array && t.Kind() != reflect.Map {
				return "request"
			}
			external += "[]"
			t = t.Elem()
		}
		if external != "" {
			path = append(path, external)
		}
	}
	if len(path) == 0 {
		return "request"
	}
	return strings.Join(path, ".")
}
