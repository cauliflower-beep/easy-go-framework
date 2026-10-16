/**
 * @Author: LiuShuXin
 * @Description: AWS S3 SDK 适配 MinIO 与其他 S3 服务
 * @File: s3.go
 * Software: Goland
 * @Date: 2026/10/15
 */
package objectstore

import (
	"context"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/cauliflower-beep/easy-go-framework/httpclient"
)

func newDriver(o Options, h *httpclient.Client) driver {
	if o.Provider == ProviderOSS {
		return newOSS(o, h)
	}
	return newS3(o, h)
}

type s3Driver struct {
	client *s3.Client
	signer *s3.PresignClient
	bucket string
}

func newS3(o Options, h *httpclient.Client) *s3Driver {
	c := s3.New(s3.Options{
		Region: o.Region, BaseEndpoint: aws.String(o.Endpoint), UsePathStyle: o.UsePathStyle, HTTPClient: h,
		Credentials:      credentials.NewStaticCredentialsProvider(o.AccessKeyID, o.AccessKeySecret, o.SessionToken),
		RetryMaxAttempts: 1,
		// 可选的 aws-chunked 校验头并非所有兼容服务都支持。保留显式/必需校验，
		// 不将 ETag 当作校验和，也不重写 SDK 的签名或数据流。
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
	return &s3Driver{client: c, signer: s3.NewPresignClient(c), bucket: o.Bucket}
}

// readerSeeker 隐藏调用方文件的 Close，防止 SDK/HTTP 关闭其文件句柄。
type readerSeeker struct{ io.ReadSeeker }

func (d *s3Driver) put(ctx context.Context, in PutInput, size int64) (ObjectInfo, error) {
	r, err := d.client.PutObject(ctx, &s3.PutObjectInput{Bucket: &d.bucket, Key: &in.Key, Body: readerSeeker{in.Body}, ContentLength: &size, ContentType: &in.ContentType, Metadata: in.Metadata})
	if err != nil {
		return ObjectInfo{}, err
	}
	return ObjectInfo{Size: size, ContentType: in.ContentType, ETag: aws.ToString(r.ETag), Metadata: cloneMetadata(in.Metadata)}, nil
}
func (d *s3Driver) get(ctx context.Context, key string) (*Object, error) {
	r, err := d.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &d.bucket, Key: &key})
	if err != nil {
		return nil, err
	}
	return &Object{ObjectInfo: ObjectInfo{Size: aws.ToInt64(r.ContentLength), ContentType: aws.ToString(r.ContentType), ETag: aws.ToString(r.ETag), LastModified: aws.ToTime(r.LastModified), Metadata: cloneMetadata(r.Metadata)}, Body: r.Body}, nil
}
func (d *s3Driver) stat(ctx context.Context, key string) (ObjectInfo, error) {
	r, err := d.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &d.bucket, Key: &key})
	if err != nil {
		return ObjectInfo{}, err
	}
	return ObjectInfo{Size: aws.ToInt64(r.ContentLength), ContentType: aws.ToString(r.ContentType), ETag: aws.ToString(r.ETag), LastModified: aws.ToTime(r.LastModified), Metadata: cloneMetadata(r.Metadata)}, nil
}
func (d *s3Driver) delete(ctx context.Context, key string) error {
	_, err := d.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &d.bucket, Key: &key})
	return err
}
func (d *s3Driver) ping(ctx context.Context) error {
	_, err := d.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &d.bucket})
	return err
}
func (d *s3Driver) sign(ctx context.Context, method, key string, expires time.Duration) (SignedRequest, error) {
	options := func(o *s3.PresignOptions) { o.Expires = expires }
	if method == "GET" {
		r, err := d.signer.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: &d.bucket, Key: &key}, options)
		if err != nil {
			return SignedRequest{}, err
		}
		return SignedRequest{URL: r.URL, Method: r.Method, Headers: r.SignedHeader.Clone(), ExpiresAt: time.Now().UTC().Truncate(time.Second).Add(expires)}, nil
	}
	r, err := d.signer.PresignPutObject(ctx, &s3.PutObjectInput{Bucket: &d.bucket, Key: &key}, options)
	if err != nil {
		return SignedRequest{}, err
	}
	return SignedRequest{URL: r.URL, Method: r.Method, Headers: r.SignedHeader.Clone(), ExpiresAt: time.Now().UTC().Truncate(time.Second).Add(expires)}, nil
}
