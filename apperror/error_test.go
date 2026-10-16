/**
 * @Author: LiuShuXin
 * @Description: 验证应用错误链、公开分类与格式化脱敏
 * @File: error_test.go
 * Software: Goland
 * @Date: 2026/10/13
 */
package apperror_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cauliflower-beep/easy-go-framework/apperror"
)

func TestErrorChainAndPublicMessage(t *testing.T) {
	cause := errors.New("private-diagnostic-value")
	err := apperror.Wrap(apperror.Conflict, apperror.Code(10003), "resource already exists", cause)
	wrapped := fmt.Errorf("application operation: %w", err)
	var public *apperror.Error
	if !errors.As(wrapped, &public) || !errors.Is(wrapped, cause) || public.Kind() != apperror.Conflict || public.Code() != apperror.Code(10003) || public.Message() != "resource already exists" {
		t.Fatal("error semantics or cause lost")
	}
	for _, format := range []string{"%s", "%v", "%+v", "%#v"} {
		if value := fmt.Sprintf(format, err); strings.Contains(value, cause.Error()) || !strings.Contains(value, fmt.Sprint(public.Code())) {
			t.Fatalf("unsafe format %s: %s", format, value)
		}
	}
	if apperror.New(apperror.NotFound, apperror.Code(10002), "resource missing").Unwrap() != nil {
		t.Fatal("new error has unexpected cause")
	}
}

func TestTypedNilErrorChain(t *testing.T) {
	var missing *apperror.Error
	var err error = missing
	if err == nil || err.Error() != "<nil>" || missing.GoString() != "<nil>" || missing.Unwrap() != nil || missing.Kind() != apperror.Internal || missing.Code() != 0 || missing.Message() != "" {
		t.Fatal("typed-nil error semantics are unsafe")
	}
	sentinel := errors.New("unrelated failure")
	wrapped := fmt.Errorf("operation: %w", err)
	if errors.Is(wrapped, nil) || errors.Is(wrapped, sentinel) {
		t.Fatal("typed-nil was treated as success or unrelated cause")
	}
	var public *apperror.Error
	if !errors.As(wrapped, &public) || public != nil {
		t.Fatal("typed-nil type identity was lost")
	}
	var other interface {
		error
		Timeout() bool
	}
	if errors.As(wrapped, &other) {
		t.Fatal("typed-nil matched an unrelated error interface")
	}
	for _, format := range []string{"%s", "%v", "%+v", "%#v"} {
		if value := fmt.Sprintf(format, missing); value != "<nil>" {
			t.Fatalf("unsafe nil formatting: %s: %s", format, value)
		}
	}
}
