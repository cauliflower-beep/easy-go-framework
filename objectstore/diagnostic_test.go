/**
 * @Author: LiuShuXin
 * @Description: 双 SDK 错误分类、凭证和对象数据诊断隔离
 * @File: diagnostic_test.go
 * Software: Goland
 * @Date: 2026/10/15
 */
package objectstore

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
	"github.com/aws/smithy-go"
	"github.com/cauliflower-beep/easy-go-framework/logging"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestErrorClassificationAndPrivacy(t *testing.T) {
	for code, expected := range map[string]error{"NoSuchKey": ErrNotFound, "NoSuchBucket": ErrBucketNotFound, "AccessDenied": ErrPermission, "InvalidAccessKeyId": ErrAuthentication, "SignatureDoesNotMatch": ErrAuthentication, "ExpiredToken": ErrAuthentication, "SlowDown": ErrUnavailable} {
		for _, raw := range []error{&oss.ServiceError{Code: code, Message: "private-message", RequestTarget: "https://host/private-key?secret"}, &smithy.GenericAPIError{Code: code, Message: "private-message"}} {
			err := SafeError(fmt.Errorf("private-wrapper: %w", raw))
			if !errors.Is(err, expected) {
				t.Errorf("classification %s: %v", code, err)
			}
			if strings.Contains(fmt.Sprintf("%v %+v %#v", err, err, err), "private-") {
				t.Error("format leaked")
			}
			entries, _ := logging.Diagnostics(err)
			if strings.Contains(fmt.Sprint(entries), "private-") {
				t.Error("diagnostic leaked")
			}
			var ae smithy.APIError
			var oe *oss.ServiceError
			if !errors.As(err, &ae) && !errors.As(err, &oe) {
				t.Error("error chain lost")
			}
		}
	}
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, ErrClosed, ErrBodyClosed} {
		if !errors.Is(SafeError(cause), cause) {
			t.Error("sentinel lost")
		}
	}
	if errors.Is(SafeError(&oss.ServiceError{StatusCode: 404}), ErrNotFound) {
		t.Error("ambiguous HEAD 404 misclassified")
	}
}

func TestOperationLogsDoNotExposeObjects(t *testing.T) {
	for _, provider := range []string{ProviderS3, ProviderOSS} {
		t.Run(provider, func(t *testing.T) {
			core, logs := observer.New(zap.DebugLevel)
			c := testClient(t, provider, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(403)
				fmt.Fprint(w, "<Error><Code>AccessDenied</Code><Message>private-message</Message></Error>")
			}, nil)
			c.logger = zap.New(core)
			_, err := c.Get(context.Background(), "private-key")
			if !errors.Is(err, ErrPermission) {
				t.Fatal(err)
			}
			if logs.Len() == 0 {
				t.Fatal("failure not logged")
			}
			for _, entry := range logs.All() {
				text := fmt.Sprint(entry.ContextMap())
				if strings.Contains(text, "private-") {
					t.Fatalf("sensitive log %s", text)
				}
			}
		})
	}
}
