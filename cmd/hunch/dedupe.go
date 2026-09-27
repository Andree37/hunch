package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/Andree37/hunch/internal/store"
)

// dedupe remembers runs by key, so a webhook delivered twice runs once.
type dedupe interface {
	// begin claims key. fresh: the caller runs it. Otherwise it is either
	// running elsewhere, or finished with prev as its reply.
	begin(ctx context.Context, key string) (prev runReply, running, fresh bool, err error)
	finish(ctx context.Context, key string, reply runReply)
	// forget drops key, e.g. after a failed run, so a retry runs again.
	forget(ctx context.Context, key string)
}

// memDedupe keeps runs in memory: one process, gone on restart.
type memDedupe struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]*dedupeEntry
}

type dedupeEntry struct {
	Running bool      `json:"running"`
	At      time.Time `json:"at"`
	Reply   runReply  `json:"reply"`
}

func newMemDedupe(ttl time.Duration) *memDedupe {
	return &memDedupe{ttl: ttl, entries: map[string]*dedupeEntry{}}
}

func (d *memDedupe) begin(_ context.Context, key string) (runReply, bool, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, e := range d.entries {
		if !e.Running && time.Since(e.At) > d.ttl {
			delete(d.entries, k)
		}
	}
	if e, ok := d.entries[key]; ok {
		return e.Reply, e.Running, false, nil
	}
	d.entries[key] = &dedupeEntry{Running: true, At: time.Now()}
	return runReply{}, false, true, nil
}

func (d *memDedupe) finish(_ context.Context, key string, reply runReply) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.entries[key] = &dedupeEntry{Reply: reply, At: time.Now()}
}

func (d *memDedupe) forget(_ context.Context, key string) {
	if key == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.entries, key)
}

// storeDedupe keeps one object per key in a directory or S3. Claiming is an
// atomic create, so every serve sharing the store agrees on who runs what,
// and the memory survives restarts.
type storeDedupe struct {
	st    store.Store
	ttl   time.Duration // how long a finished run's reply is kept
	stale time.Duration // a "running" claim older than this was abandoned
}

func objectKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:]) + ".json"
}

func (d *storeDedupe) begin(ctx context.Context, key string) (runReply, bool, bool, error) {
	claim, _ := json.Marshal(dedupeEntry{Running: true, At: time.Now().UTC()})
	obj := objectKey(key)
	for attempt := 0; attempt < 2; attempt++ {
		err := d.st.Create(ctx, obj, claim)
		if err == nil {
			return runReply{}, false, true, nil
		}
		if !errors.Is(err, store.ErrExists) {
			return runReply{}, false, false, err
		}
		data, err := d.st.Get(ctx, obj)
		if errors.Is(err, store.ErrNotFound) {
			continue // forgotten in between: try to claim again
		}
		if err != nil {
			return runReply{}, false, false, err
		}
		var e dedupeEntry
		if json.Unmarshal(data, &e) != nil {
			return runReply{}, false, false, errors.New("unreadable entry for " + key)
		}
		expired := (e.Running && time.Since(e.At) > d.stale) || (!e.Running && time.Since(e.At) > d.ttl)
		if !expired {
			return e.Reply, e.Running, false, nil
		}
		// Old enough to drop; whoever re-creates it first wins.
		if err := d.st.Delete(ctx, obj); err != nil {
			return runReply{}, false, false, err
		}
	}
	return runReply{}, true, false, nil // lost the race to re-claim it
}

func (d *storeDedupe) finish(ctx context.Context, key string, reply runReply) {
	data, _ := json.Marshal(dedupeEntry{At: time.Now().UTC(), Reply: reply})
	if err := d.st.Put(ctx, objectKey(key), data); err != nil {
		logf("dedupe: remembering %s: %v", key, err)
	}
}

func (d *storeDedupe) forget(ctx context.Context, key string) {
	if key == "" {
		return
	}
	if err := d.st.Delete(ctx, objectKey(key)); err != nil {
		logf("dedupe: forgetting %s: %v", key, err)
	}
}
