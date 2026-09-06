package cache

import (
	"container/list"
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Memory is an LRU response cache bounded by MaxEntries. Eviction is
// strict LRU on insert past capacity; entries past their TTL are
// expired lazily on Get and swept periodically (every 5 min).
//
// Safe for concurrent use. Stats are atomic so the dashboard can
// poll without grabbing the mutex.
type Memory struct {
	maxEntries int
	defaultTTL time.Duration
	mu         sync.Mutex
	items      map[string]*list.Element
	order      *list.List

	hits     atomic.Int64
	misses   atomic.Int64
	evicted  atomic.Int64
	bytes    atomic.Int64
	stopOnce sync.Once
	stop     chan struct{}
}

type entry struct {
	key       string
	value     []byte
	expiresAt time.Time
}

// NewMemory returns a started memory cache. A sweeper goroutine runs
// every 5 min to drop expired entries; Close stops it.
func NewMemory(maxEntries int, defaultTTL time.Duration) *Memory {
	if maxEntries <= 0 {
		maxEntries = 1000
	}
	if defaultTTL <= 0 {
		defaultTTL = 24 * time.Hour
	}
	m := &Memory{
		maxEntries: maxEntries,
		defaultTTL: defaultTTL,
		items:      make(map[string]*list.Element),
		order:      list.New(),
		stop:       make(chan struct{}),
	}
	go m.sweepLoop()
	return m
}

func (m *Memory) Get(_ context.Context, key string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	el, ok := m.items[key]
	if !ok {
		m.misses.Add(1)
		return nil, false
	}
	e := el.Value.(*entry)
	if !e.expiresAt.IsZero() && time.Now().After(e.expiresAt) {
		m.removeLocked(el)
		m.misses.Add(1)
		return nil, false
	}
	m.order.MoveToFront(el)
	m.hits.Add(1)
	// Copy the value so the caller can't mutate the cached bytes.
	out := make([]byte, len(e.value))
	copy(out, e.value)
	return out, true
}

func (m *Memory) Set(_ context.Context, key string, value []byte, ttl time.Duration) {
	if ttl <= 0 {
		ttl = m.defaultTTL
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	expiresAt := time.Now().Add(ttl)
	if el, ok := m.items[key]; ok {
		old := el.Value.(*entry)
		m.bytes.Add(-int64(len(old.value)))
		old.value = append(old.value[:0], value...)
		old.expiresAt = expiresAt
		m.bytes.Add(int64(len(old.value)))
		m.order.MoveToFront(el)
		return
	}
	e := &entry{key: key, value: append([]byte(nil), value...), expiresAt: expiresAt}
	el := m.order.PushFront(e)
	m.items[key] = el
	m.bytes.Add(int64(len(e.value)))
	// Evict if over capacity. LRU = tail of the list.
	for len(m.items) > m.maxEntries {
		tail := m.order.Back()
		if tail == nil {
			break
		}
		m.removeLocked(tail)
		m.evicted.Add(1)
	}
}

func (m *Memory) Delete(_ context.Context, key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if el, ok := m.items[key]; ok {
		m.removeLocked(el)
	}
}

func (m *Memory) DeleteNamespace(_ context.Context, ns string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prefix := ns + "/"
	for k, el := range m.items {
		if strings.HasPrefix(k, prefix) {
			m.removeLocked(el)
		}
	}
}

func (m *Memory) DeleteAll(_ context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items = make(map[string]*list.Element)
	m.order.Init()
	m.bytes.Store(0)
}

func (m *Memory) Stats() Stats {
	m.mu.Lock()
	entries := len(m.items)
	m.mu.Unlock()
	return Stats{
		Driver:       "memory",
		Entries:      entries,
		BytesStored:  m.bytes.Load(),
		HitsTotal:    m.hits.Load(),
		MissesTotal:  m.misses.Load(),
		EvictedTotal: m.evicted.Load(),
	}
}

func (m *Memory) Close() error {
	m.stopOnce.Do(func() { close(m.stop) })
	return nil
}

// removeLocked drops `el` from both the map and the list. Caller
// already holds m.mu.
func (m *Memory) removeLocked(el *list.Element) {
	e := el.Value.(*entry)
	delete(m.items, e.key)
	m.order.Remove(el)
	m.bytes.Add(-int64(len(e.value)))
}

func (m *Memory) sweepLoop() {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
			m.sweep()
		}
	}
}

func (m *Memory) sweep() {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, el := range m.items {
		e := el.Value.(*entry)
		if !e.expiresAt.IsZero() && now.After(e.expiresAt) {
			m.removeLocked(el)
			m.evicted.Add(1)
			_ = k
		}
	}
}
