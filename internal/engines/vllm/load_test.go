package vllm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLoadScrapesVLLMMetrics(t *testing.T) {
	gen := 1000.0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("vllm:gpu_cache_usage_perc{model_name=\"m\"} 0.375\nvllm:num_requests_waiting{model_name=\"m\"} 2\n" +
			"vllm:generation_tokens_total{model_name=\"m\"} " + trim(gen) + "\nvllm:prefix_cache_hits_total{model_name=\"m\"} 30\nvllm:prefix_cache_queries_total{model_name=\"m\"} 40\n"))
		gen += 500
	}))
	defer srv.Close()
	d := New(srv.URL, "")
	ld, err := d.Load(context.Background())
	if err != nil || ld.KVUsedPct != 37.5 || ld.QueueDepth != 2 || ld.PrefixHitPct != 75 || ld.SampledAt == 0 {
		t.Fatalf("first sample %+v %v", ld, err)
	}
	if ld.TokensPerSec != 0 {
		t.Fatalf("first sample has no rate, got %v", ld.TokensPerSec)
	}
	ld, _ = d.Load(context.Background())
	if ld.TokensPerSec <= 0 {
		t.Fatalf("second sample must carry a generation rate, got %+v", ld)
	}
}

func trim(f float64) string {
	s := ""
	n := int64(f)
	if n == 0 {
		return "0"
	}
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}
