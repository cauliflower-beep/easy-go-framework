/**
 * @Author: LiuShuXin
 * @Description: 阿里云 OSS 官方 SDK 适配与原生签名
 * @File: oss.go
 * Software: Goland
 * @Date: 2026/10/15
 */
package objectstore

import (
	"context"
	"net/http"
	"time"

	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss/credentials"
	"github.com/cauliflower-beep/easy-go-framework/httpclient"
)

type ossDriver struct {
	client *oss.Client
	bucket string
}

func newOSS(o Options, h *httpclient.Client) *ossDriver {
	cfg := (&oss.Config{}).WithRegion(o.Region).WithEndpoint(o.Endpoint).
		WithSignatureVersion(oss.SignatureVersionV4).
		WithCredentialsProvider(credentials.NewStaticCredentialsProvider(o.AccessKeyID, o.AccessKeySecret, o.SessionToken)).
		WithRetryMaxAttempts(1).WithUsePathStyle(false).WithEnabledRedirect(false).WithLogLevel(0)
	cfg.HttpClient = h
	return &ossDriver{client: oss.NewClient(cfg), bucket: o.Bucket}
}
func (d *ossDriver) put(ctx context.Context, in PutInput, size int64) (ObjectInfo, error) {
	r, err := d.client.PutObject(ctx, &oss.PutObjectRequest{Bucket: &d.bucket, Key: &in.Key, Body: readerSeeker{in.Body}, ContentLength: &size, ContentType: &in.ContentType, Metadata: in.Metadata})
	if err != nil {
		return ObjectInfo{}, err
	}
	return ObjectInfo{Size: size, ContentType: in.ContentType, ETag: oss.ToString(r.ETag), Metadata: cloneMetadata(in.Metadata)}, nil
}
func (d *ossDriver) get(ctx context.Context, key string) (*Object, error) {
	r, err := d.client.GetObject(ctx, &oss.GetObjectRequest{Bucket: &d.bucket, Key: &key})
	if err != nil {
		return nil, err
	}
	return &Object{ObjectInfo: ObjectInfo{Size: r.ContentLength, ContentType: oss.ToString(r.ContentType), ETag: oss.ToString(r.ETag), LastModified: oss.ToTime(r.LastModified), Metadata: cloneMetadata(r.Metadata)}, Body: r.Body}, nil
}
func (d *ossDriver) stat(ctx context.Context, key string) (ObjectInfo, error) {
	r, err := d.client.HeadObject(ctx, &oss.HeadObjectRequest{Bucket: &d.bucket, Key: &key})
	if err != nil {
		return ObjectInfo{}, err
	}
	return ObjectInfo{Size: r.ContentLength, ContentType: oss.ToString(r.ContentType), ETag: oss.ToString(r.ETag), LastModified: oss.ToTime(r.LastModified), Metadata: cloneMetadata(r.Metadata)}, nil
}
func (d *ossDriver) delete(ctx context.Context, key string) error {
	_, err := d.client.DeleteObject(ctx, &oss.DeleteObjectRequest{Bucket: &d.bucket, Key: &key})
	return err
}
func (d *ossDriver) ping(ctx context.Context) error {
	_, err := d.client.GetBucketInfo(ctx, &oss.GetBucketInfoRequest{Bucket: &d.bucket})
	return err
}
func (d *ossDriver) sign(ctx context.Context, method, key string, expires time.Duration) (SignedRequest, error) {
	var request any = &oss.GetObjectRequest{Bucket: &d.bucket, Key: &key}
	if method == "PUT" {
		request = &oss.PutObjectRequest{Bucket: &d.bucket, Key: &key}
	}
	r, err := d.client.Presign(ctx, request, func(o *oss.PresignOptions) { o.Expires = expires })
	if err != nil {
		return SignedRequest{}, err
	}
	headers := make(http.Header, len(r.SignedHeaders))
	for k, v := range r.SignedHeaders {
		headers.Set(k, v)
	}
	return SignedRequest{URL: r.URL, Method: r.Method, Headers: headers, ExpiresAt: r.Expiration}, nil
}
