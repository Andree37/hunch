package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// s3API is the part of the S3 client the store uses, so tests can fake it.
type s3API interface {
	PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	DeleteObject(ctx context.Context, in *s3.DeleteObjectInput, opts ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, opts ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
}

// S3 stores each key as an object under bucket/prefix. Create uses S3's
// conditional write (If-None-Match: *), so it is atomic across every process
// writing to the bucket. Credentials and region come from the usual AWS
// chain; ?region= on the location overrides the region.
type S3 struct {
	api    s3API
	bucket string
	prefix string
}

func openS3(ctx context.Context, loc string) (*S3, error) {
	u, err := url.Parse(loc)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("bad S3 location %q (want s3://bucket/prefix)", loc)
	}
	var opts []func(*config.LoadOptions) error
	if r := u.Query().Get("region"); r != "" {
		opts = append(opts, config.WithRegion(r))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("AWS config: %w", err)
	}
	return &S3{api: s3.NewFromConfig(cfg), bucket: u.Host, prefix: cleanPrefix(u.Path)}, nil
}

func cleanPrefix(p string) string {
	p = strings.Trim(p, "/")
	if p == "" {
		return ""
	}
	return p + "/"
}

func (s *S3) key(k string) *string { return aws.String(s.prefix + k) }

func (s *S3) Create(ctx context.Context, key string, data []byte) error {
	_, err := s.api.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket), Key: s.key(key), Body: bytes.NewReader(data),
		IfNoneMatch: aws.String("*"),
	})
	// 412: someone else created it first. 409: a concurrent create is in
	// flight; either way it isn't ours.
	if code := apiCode(err); code == "PreconditionFailed" || code == "ConditionalRequestConflict" {
		return ErrExists
	}
	return err
}

func (s *S3) Put(ctx context.Context, key string, data []byte) error {
	_, err := s.api.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(s.bucket), Key: s.key(key), Body: bytes.NewReader(data)})
	return err
}

func (s *S3) Get(ctx context.Context, key string) ([]byte, error) {
	out, err := s.api.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: s.key(key)})
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) || apiCode(err) == "NoSuchKey" {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	defer out.Body.Close()
	return io.ReadAll(out.Body)
}

func (s *S3) Delete(ctx context.Context, key string) error {
	_, err := s.api.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: s.key(key)})
	return err
}

func (s *S3) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	var token *string
	for {
		out, err := s.api.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket: aws.String(s.bucket), Prefix: s.key(prefix), ContinuationToken: token,
		})
		if err != nil {
			return nil, err
		}
		for _, o := range out.Contents {
			keys = append(keys, strings.TrimPrefix(aws.ToString(o.Key), s.prefix))
		}
		if !aws.ToBool(out.IsTruncated) {
			return keys, nil // S3 lists keys in order already
		}
		token = out.NextContinuationToken
	}
}

func (s *S3) String() string { return "s3://" + s.bucket + "/" + s.prefix }

func apiCode(err error) string {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		return ae.ErrorCode()
	}
	return ""
}
