package store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// fakeS3 behaves like S3 for the calls the store makes, including
// conditional writes.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func (f *fakeS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	data, _ := io.ReadAll(in.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	key := aws.ToString(in.Key)
	if _, ok := f.objects[key]; ok && aws.ToString(in.IfNoneMatch) == "*" {
		return nil, &smithy.GenericAPIError{Code: "PreconditionFailed"}
	}
	f.objects[key] = data
	return &s3.PutObjectOutput{}, nil
}

func (f *fakeS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.objects[aws.ToString(in.Key)]
	if !ok {
		return nil, &types.NoSuchKey{}
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(data))}, nil
}

func (f *fakeS3) DeleteObject(_ context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, aws.ToString(in.Key))
	return &s3.DeleteObjectOutput{}, nil
}

func (f *fakeS3) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, aws.ToString(in.Prefix)) {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	out := &s3.ListObjectsV2Output{IsTruncated: aws.Bool(false)}
	for _, k := range keys {
		out.Contents = append(out.Contents, types.Object{Key: aws.String(k)})
	}
	return out, nil
}

func stores(t *testing.T) map[string]Store {
	return map[string]Store{
		"dir": Dir(t.TempDir()),
		"s3":  &S3{api: &fakeS3{objects: map[string][]byte{}}, bucket: "b", prefix: "hunch/"},
	}
}

func TestContract(t *testing.T) {
	ctx := context.Background()
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			if _, err := s.Get(ctx, "a/x"); !errors.Is(err, ErrNotFound) {
				t.Errorf("get missing: %v", err)
			}
			if err := s.Create(ctx, "a/x", []byte("one")); err != nil {
				t.Fatal(err)
			}
			if err := s.Create(ctx, "a/x", []byte("two")); !errors.Is(err, ErrExists) {
				t.Errorf("second create: %v", err)
			}
			if got, _ := s.Get(ctx, "a/x"); string(got) != "one" {
				t.Errorf("get = %q", got)
			}
			s.Put(ctx, "a/x", []byte("three"))
			s.Put(ctx, "a/y", []byte("y"))
			s.Put(ctx, "b/z", []byte("z"))
			if got, _ := s.Get(ctx, "a/x"); string(got) != "three" {
				t.Errorf("after put = %q", got)
			}
			if keys, _ := s.List(ctx, "a/"); !slices.Equal(keys, []string{"a/x", "a/y"}) {
				t.Errorf("list = %v", keys)
			}
			s.Delete(ctx, "a/x")
			if err := s.Delete(ctx, "a/x"); err != nil {
				t.Errorf("deleting twice: %v", err)
			}
			if err := s.Create(ctx, "a/x", []byte("again")); err != nil {
				t.Errorf("create after delete: %v", err)
			}
		})
	}
}

func TestCreateHasOneWinner(t *testing.T) {
	ctx := context.Background()
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			var wins atomic.Int32
			var wg sync.WaitGroup
			for i := 0; i < 20; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if s.Create(ctx, "race", []byte("x")) == nil {
						wins.Add(1)
					}
				}()
			}
			wg.Wait()
			if wins.Load() != 1 {
				t.Errorf("winners = %d, want 1", wins.Load())
			}
		})
	}
}

func TestOpen(t *testing.T) {
	d := t.TempDir() + "/nested/dir"
	s, err := Open(context.Background(), d)
	if err != nil || s.String() != d {
		t.Fatalf("open dir: %v, %v", s, err)
	}
	if !IsRemote("s3://b/p") || IsRemote("./runs") {
		t.Error("IsRemote disagrees")
	}
	if _, err := Open(context.Background(), "s3:///nobucket"); err == nil {
		t.Error("expected error for missing bucket")
	}
	if cleanPrefix("/runs/") != "runs/" || cleanPrefix("") != "" {
		t.Error("cleanPrefix")
	}
}
