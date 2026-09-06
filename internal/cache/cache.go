// Package cache provides a response cache for deterministic
// inference operations (embeddings today; chat completions to follow
// once the streaming-replay design is settled).
//
// Two drivers are shipped: an in-memory LRU (default) and a
// SQLite-backed persistent driver that reuses the existing
// `~/.opod/state.db`. Per the planning doc no external services
// (Redis, etc.) are in scope — Opod is per-host.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"
)

// Cache is the small interface every driver implements. All values are
// opaque bytes; the calling handler is responsible for marshalling.
//
// TTL of zero on Set means "use the driver's default". Drivers MUST
// expire entries lazily on Get (return ok=false past expiry); the
// memory driver also runs a periodic sweeper for hard-bounded
// capacity. SQLite reaps via a periodic DELETE.
type Cache interface {
	Get(ctx context.Context, key string) ([]byte, bool)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration)
	Delete(ctx context.Context, key string)
	// DeleteNamespace clears every entry whose key starts with the
	// given namespace prefix. Used by the admin endpoint.
	DeleteNamespace(ctx context.Context, ns string)
	// DeleteAll drops every cached entry regardless of namespace.
	// Backs the admin endpoint's `?all=1` flush.
	DeleteAll(ctx context.Context)
	Stats() Stats
	// Close stops any background goroutines (memory sweeper, sqlite
	// reaper). Idempotent.
	Close() error
}

// Stats is the small metrics view exposed via /admin/v1/cache/stats.
type Stats struct {
	Driver       string `json:"driver"`
	Entries      int    `json:"entries"`
	BytesStored  int64  `json:"bytes_stored"`
	HitsTotal    int64  `json:"hits_total"`
	MissesTotal  int64  `json:"misses_total"`
	EvictedTotal int64  `json:"evicted_total"`
}

// KeyForRequest builds a deterministic cache key from the request
// path + a JSON body. Path is included so an embeddings request and a
// chat request with identical bodies (somehow) wouldn't collide.
//
// The body is canonicalized — fields sorted alphabetically, ephemeral
// fields removed — so semantically-equal requests collide even when
// the encoded byte order differs. `opod.cache.namespace` survives
// canonicalization (it's the explicit operator-side bucket).
func KeyForRequest(path string, body []byte, namespace string) string {
	canon := canonicalize(body)
	h := sha256.New()
	h.Write([]byte(namespace))
	h.Write([]byte{0})
	h.Write([]byte(path))
	h.Write([]byte{0})
	h.Write(canon)
	full := hex.EncodeToString(h.Sum(nil))
	if namespace != "" {
		return namespace + "/" + full
	}
	return full
}

// canonicalize returns a stable byte representation of `body`. Object
// keys are sorted; arrays preserved as-is. Ephemeral fields (user,
// request_id-equivalents) that some clients set are NOT stripped
// today — the planning doc calls this out as a v1 known-limitation;
// the key set is small enough that operators can tweak the normalizer
// list here when a real case surfaces.
func canonicalize(body []byte) []byte {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return body
	}
	v = stripEphemeral(v)
	b, err := json.Marshal(sortObject(v))
	if err != nil {
		return body
	}
	return b
}

func stripEphemeral(v any) any {
	if m, ok := v.(map[string]any); ok {
		out := make(map[string]any, len(m))
		for k, v := range m {
			switch k {
			case "user", "stream_options":
				// Skip — these don't affect the response shape.
			default:
				out[k] = stripEphemeral(v)
			}
		}
		return out
	}
	if a, ok := v.([]any); ok {
		for i := range a {
			a[i] = stripEphemeral(a[i])
		}
		return a
	}
	return v
}

func sortObject(v any) any {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make(map[string]json.RawMessage, len(keys))
		for _, k := range keys {
			b, err := json.Marshal(sortObject(t[k]))
			if err != nil {
				continue
			}
			out[k] = b
		}
		// json.Marshal of map[string]X iterates in sorted-key order
		// thanks to encoding/json's guarantee, so reusing the map here
		// gives the same canonical byte string.
		return out
	case []any:
		for i := range t {
			t[i] = sortObject(t[i])
		}
		return t
	default:
		return t
	}
}
