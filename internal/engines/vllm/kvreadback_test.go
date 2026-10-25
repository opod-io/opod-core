package vllm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// PLAN T23.2: the KV pool vLLM really built is num_gpu_blocks × block_size
// from its cache_config_info — an info metric, so the numbers are LABELS — and
// a data-parallel server publishes one series per engine, which sum. Its
// preemptions are a counter. vLLM counts no tokens in use and no evictions, so
// those are absent, never zero.
func TestLoadReadsTheKVPoolVLLMBuilt(t *testing.T) {
	body := `# HELP vllm:cache_config_info Information of the LLMEngine CacheConfig
# TYPE vllm:cache_config_info gauge
vllm:cache_config_info{block_size="16",cache_dtype="auto",engine="0",gpu_memory_utilization="0.85",num_gpu_blocks="2048",num_cpu_blocks="None"} 1.0
vllm:cache_config_info{block_size="16",cache_dtype="auto",engine="1",gpu_memory_utilization="0.85",num_gpu_blocks="1024",num_cpu_blocks="None"} 1.0
vllm:kv_cache_usage_perc{engine="0",model_name="m"} 0.25
vllm:num_preemptions_total{engine="0",model_name="m"} 3.0
vllm:num_preemptions_total{engine="1",model_name="m"} 2.0
`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
	defer srv.Close()
	ld, err := New(srv.URL, "").Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ld.KVCapacityTokens == nil || *ld.KVCapacityTokens != (2048+1024)*16 {
		t.Fatalf("pool: %v, want %d tokens", ld.KVCapacityTokens, (2048+1024)*16)
	}
	if ld.PreemptionsTotal == nil || *ld.PreemptionsTotal != 5 {
		t.Fatalf("preemptions: %v, want 5", ld.PreemptionsTotal)
	}
	if ld.KVUsedTokens != nil || ld.EvictionsTotal != nil {
		t.Fatalf("vLLM does not say these: used %v evictions %v", ld.KVUsedTokens, ld.EvictionsTotal)
	}
	if ld.KVUsedPct != 25 {
		t.Fatalf("the pressure signal is unchanged: %v", ld.KVUsedPct)
	}
}

// PLAN T26.4: a worker with a host-memory tier publishes its hits and queries
// (counters, so _total), the bytes moved each way, its fill as a fraction and
// its refused stores; data-parallel series sum. A worker with no tier states
// none of them.
func TestLoadReadsTheHostTier(t *testing.T) {
	body := `vllm:external_prefix_cache_hits_total{engine="0",model_name="m"} 1200.0
vllm:external_prefix_cache_hits_total{engine="1",model_name="m"} 300.0
vllm:external_prefix_cache_queries_total{engine="0",model_name="m"} 2000.0
vllm:kv_offload_load_bytes_total{engine="0"} 5.24288e+06
vllm:kv_offload_store_bytes_total{engine="0"} 1.048576e+07
vllm:kv_offload_cpu_cache_usage_perc{engine="0"} 0.375
vllm:kv_offload_allocation_failure_total{engine="0"} 2.0
`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
	defer srv.Close()
	ld, err := New(srv.URL, "").Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		got  *int64
		want int64
	}{
		"hits": {ld.TierHitTokensTotal, 1500}, "queries": {ld.TierQueryTokensTotal, 2000},
		"loaded": {ld.TierLoadBytesTotal, 5242880}, "stored": {ld.TierStoreBytesTotal, 10485760},
		"failures": {ld.TierAllocFailuresTotal, 2},
	} {
		if c.got == nil || *c.got != c.want {
			t.Errorf("%s: %v, want %d", name, c.got, c.want)
		}
	}
	if ld.TierUsedPct == nil || *ld.TierUsedPct != 37.5 {
		t.Errorf("fill: %v, want 37.5", ld.TierUsedPct)
	}

	none := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("vllm:kv_cache_usage_perc 0.1\n"))
	}))
	defer none.Close()
	ld, err = New(none.URL, "").Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ld.TierHitTokensTotal != nil || ld.TierQueryTokensTotal != nil || ld.TierLoadBytesTotal != nil ||
		ld.TierStoreBytesTotal != nil || ld.TierUsedPct != nil || ld.TierAllocFailuresTotal != nil {
		t.Fatalf("no tier, nothing stated: %+v", ld)
	}
}

// Before its profile run vLLM has not sized the pool (num_gpu_blocks "None"),
// and an older build publishes no info metric: no pool is stated either way.
func TestNoKVPoolUntilVLLMSaysOne(t *testing.T) {
	for name, body := range map[string]string{
		"not sized yet":  `vllm:cache_config_info{block_size="16",engine="0",num_gpu_blocks="None"} 1.0` + "\n",
		"no info metric": "vllm:kv_cache_usage_perc 0.1\n",
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
		ld, err := New(srv.URL, "").Load(context.Background())
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		if ld.KVCapacityTokens != nil || ld.PreemptionsTotal != nil {
			t.Errorf("%s: pool %v preemptions %v, want neither", name, ld.KVCapacityTokens, ld.PreemptionsTotal)
		}
	}
}
