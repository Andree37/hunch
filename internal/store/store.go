// Package store keeps small objects as files under a directory. Recorded
// runs are built on it: one file per run.
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

var ErrNotFound = errors.New("not found")

type Store interface {
	Put(ctx context.Context, key string, data []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
	Delete(ctx context.Context, key string) error
	// List returns the keys under prefix, sorted.
	List(ctx context.Context, prefix string) ([]string, error)
	String() string
}

// Open returns the store for a directory, created if missing.
func Open(_ context.Context, loc string) (Store, error) {
	if err := os.MkdirAll(loc, 0o755); err != nil {
		return nil, err
	}
	return Dir(loc), nil
}

// Dir stores each key as a file under a directory.
type Dir string

func (d Dir) path(key string) string { return filepath.Join(string(d), filepath.FromSlash(key)) }

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
