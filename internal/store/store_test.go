package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestDir(t *testing.T) {
	ctx := context.Background()
	s := Dir(t.TempDir())
	if _, err := s.Get(ctx, "a/x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("get missing: %v", err)
	}
	s.Put(ctx, "a/x", []byte("one"))
	s.Put(ctx, "a/x", []byte("two"))
	s.Put(ctx, "a/y", []byte("y"))
	s.Put(ctx, "b/z", []byte("z"))
	if got, _ := s.Get(ctx, "a/x"); string(got) != "two" {
		t.Errorf("after put = %q", got)
	}
	if keys, _ := s.List(ctx, "a/"); !slices.Equal(keys, []string{"a/x", "a/y"}) {
		t.Errorf("list = %v", keys)
	}
	s.Delete(ctx, "a/x")
	if err := s.Delete(ctx, "a/x"); err != nil {
		t.Errorf("deleting twice: %v", err)
	}
	if keys, _ := s.List(ctx, ""); !slices.Equal(keys, []string{"a/y", "b/z"}) {
		t.Errorf("list after delete = %v", keys)
	}
}

func TestOpenMakesTheDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runs")
	if _, err := Open(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Errorf("%s: %v", dir, err)
	}
}
