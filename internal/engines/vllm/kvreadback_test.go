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
