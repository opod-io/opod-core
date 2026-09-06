package cache

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/opod-io/opod/internal/store"
)

// SQLite wraps a store.CacheStore in the cache.Cache interface so
// callers don't have to know which driver they're using. Hit / miss
// counters are in-process (the same as the memory driver); the
// underlying SQLite COUNT(*) is exposed as Entries via Stats().
type SQLite struct {
	backend    store.CacheStore
	defaultTTL time.Duration
	hits       atomic.Int64
	misses     atomic.Int64
	stopOnce   sync.Once
	stop       chan struct{}
}

// NewSQLite returns a started SQLite cache driver. A reaper goroutine
// runs every 5 min to DELETE expired rows; Close stops it.
func NewSQLite(backend store.CacheStore, defaultTTL time.Duration) *SQLite {
	if defaultTTL <= 0 {
		defaultTTL = 24 * time.Hour
	}
	c := &SQLite{
		backend:    backend,
		defaultTTL: defaultTTL,
		stop:       make(chan struct{}),
	}
	go c.reaperLoop()
	return c
}

func (c *SQLite) Get(ctx context.Context, key string) ([]byte, bool) {
	v, ok, err := c.backend.Get(ctx, key)
	if err != nil || !ok {
		c.misses.Add(1)
		return nil, false
	}
	c.hits.Add(1)
	return v, true
}

func (c *SQLite) Set(ctx context.Context, key string, value []byte, ttl time.Duration) {
	if ttl <= 0 {
		ttl = c.defaultTTL
	}
	ns := ""
	if i := strings.Index(key, "/"); i > 0 {
		ns = key[:i]
	}
	_ = c.backend.Set(ctx, key, ns, value, time.Now().Add(ttl))
}

func (c *SQLite) Delete(ctx context.Context, key string) {
	_ = c.backend.Delete(ctx, key)
}

func (c *SQLite) DeleteNamespace(ctx context.Context, ns string) {
	_ = c.backend.DeleteNamespace(ctx, ns)
}

func (c *SQLite) DeleteAll(ctx context.Context) {
	_ = c.backend.DeleteAll(ctx)
}

func (c *SQLite) Stats() Stats {
	n, b, _ := c.backend.Count(context.Background())
	return Stats{
		Driver:      "sqlite",
		Entries:     n,
		BytesStored: b,
		HitsTotal:   c.hits.Load(),
		MissesTotal: c.misses.Load(),
	}
}

func (c *SQLite) Close() error {
	c.stopOnce.Do(func() { close(c.stop) })
	return nil
}

func (c *SQLite) reaperLoop() {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-t.C:
			_, _ = c.backend.SweepExpired(context.Background(), time.Now())
		}
	}
}
