package cache

import (
	"context"
	"testing"
	"time"
)

func TestMemory_HitMissTTL(t *testing.T) {
	c := NewMemory(10, 50*time.Millisecond)
	defer c.Close()
	ctx := context.Background()

	c.Set(ctx, "k1", []byte("v1"), 0)
	v, ok := c.Get(ctx, "k1")
	if !ok || string(v) != "v1" {
		t.Fatalf("Get hit failed: ok=%v v=%q", ok, v)
	}
	// Miss path.
	if _, ok := c.Get(ctx, "nope"); ok {
		t.Error("Get for missing key should be miss")
	}

	time.Sleep(80 * time.Millisecond)
	if _, ok := c.Get(ctx, "k1"); ok {
		t.Error("k1 should have expired by now")
	}
}

func TestMemory_LRUEviction(t *testing.T) {
	c := NewMemory(2, time.Hour) // tiny cap
	defer c.Close()
	ctx := context.Background()
	c.Set(ctx, "a", []byte("1"), 0)
	c.Set(ctx, "b", []byte("2"), 0)
	c.Set(ctx, "c", []byte("3"), 0) // evicts a (LRU)
	if _, ok := c.Get(ctx, "a"); ok {
		t.Error("a should have been evicted")
	}
	for _, k := range []string{"b", "c"} {
		if _, ok := c.Get(ctx, k); !ok {
			t.Errorf("%s should still be present", k)
		}
	}
	stats := c.Stats()
	if stats.EvictedTotal != 1 {
		t.Errorf("evicted = %d, want 1", stats.EvictedTotal)
	}
}

func TestKeyForRequest_CanonicalOrderStable(t *testing.T) {
	// Two semantically-equal bodies with different key ordering
	// must produce the same key.
	a := []byte(`{"model":"x","input":["hello","world"]}`)
	b := []byte(`{"input":["hello","world"],"model":"x"}`)
	if KeyForRequest("/v1/embeddings", a, "") != KeyForRequest("/v1/embeddings", b, "") {
		t.Error("canonical normalizer should make these the same key")
	}
}

func TestKeyForRequest_DifferentBodiesDiffer(t *testing.T) {
	a := []byte(`{"model":"x","input":["hello"]}`)
	b := []byte(`{"model":"x","input":["hello world"]}`)
	if KeyForRequest("/v1/embeddings", a, "") == KeyForRequest("/v1/embeddings", b, "") {
		t.Error("different inputs should give different keys")
	}
}

func TestKeyForRequest_NamespaceIsolation(t *testing.T) {
	a := []byte(`{"model":"x","input":["hi"]}`)
	if KeyForRequest("/v1/embeddings", a, "team-A") == KeyForRequest("/v1/embeddings", a, "team-B") {
		t.Error("different namespaces should give different keys")
	}
	// Namespace is also a key prefix so DeleteNamespace can target.
	k := KeyForRequest("/v1/embeddings", a, "team-A")
	if k[:7] != "team-A/" {
		t.Errorf("namespace should be a key prefix: %q", k)
	}
}

func TestMemory_DeleteNamespace(t *testing.T) {
	c := NewMemory(20, time.Hour)
	defer c.Close()
	ctx := context.Background()
	c.Set(ctx, "ns1/k1", []byte("1"), 0)
	c.Set(ctx, "ns1/k2", []byte("2"), 0)
	c.Set(ctx, "ns2/k3", []byte("3"), 0)
	c.DeleteNamespace(ctx, "ns1")
	for _, k := range []string{"ns1/k1", "ns1/k2"} {
		if _, ok := c.Get(ctx, k); ok {
			t.Errorf("%s should have been deleted", k)
		}
	}
	if _, ok := c.Get(ctx, "ns2/k3"); !ok {
		t.Error("ns2/k3 should still be present")
	}
}
