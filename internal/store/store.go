// Package store keeps small objects either in a local directory or in S3,
// chosen by the location string: "s3://bucket/prefix" or a path. Recordings
// and serve's duplicate memory are built on it, so both can live on disk or
// in S3 without caring which.
package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

var (
	ErrNotFound = errors.New("not found")
	ErrExists   = errors.New("already exists")
)

type Store interface {
	// Create writes key only if it doesn't exist yet, atomically: of several
	// writers racing for the same key, exactly one succeeds; the others get
	// ErrExists.
	Create(ctx context.Context, key string, data []byte) error
	Put(ctx context.Context, key string, data []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
	Delete(ctx context.Context, key string) error
	// List returns the keys under prefix, sorted.
	List(ctx context.Context, prefix string) ([]string, error)
	String() string
}

// IsRemote reports whether a location names S3 rather than the disk.
func IsRemote(loc string) bool { return strings.HasPrefix(loc, "s3://") }

// Open returns the store for a location: "s3://bucket/prefix" or a directory
// (created if missing).
func Open(ctx context.Context, loc string) (Store, error) {
	if IsRemote(loc) {
		return openS3(ctx, loc)
	}
	if err := os.MkdirAll(loc, 0o755); err != nil {
		return nil, err
	}
	return Dir(loc), nil
}

// Dir stores each key as a file under a directory. Create uses O_EXCL, so it
// is atomic across processes sharing the directory (including over NFS/EFS).
type Dir string

func (d Dir) path(key string) string { return filepath.Join(string(d), filepath.FromSlash(key)) }

func (d Dir) Create(_ context.Context, key string, data []byte) error {
	p := d.path(key)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, fs.ErrExist) {
		return ErrExists
	}
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (d Dir) Put(_ context.Context, key string, data []byte) error {
	p := d.path(key)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	// Write then rename, so readers never see half a file.
	tmp := fmt.Sprintf("%s.tmp-%d", p, os.Getpid())
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (d Dir) Get(_ context.Context, key string) ([]byte, error) {
	data, err := os.ReadFile(d.path(key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	return data, err
}

func (d Dir) Delete(_ context.Context, key string) error {
	err := os.Remove(d.path(key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func (d Dir) List(_ context.Context, prefix string) ([]string, error) {
	var keys []string
	err := filepath.WalkDir(string(d), func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() || strings.Contains(e.Name(), ".tmp-") {
			return err
		}
		rel, err := filepath.Rel(string(d), p)
		if err != nil {
			return err
		}
		if key := filepath.ToSlash(rel); strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	slices.Sort(keys)
	return keys, err
}

func (d Dir) String() string { return string(d) }
