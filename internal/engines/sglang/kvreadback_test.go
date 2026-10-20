package sglang

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// PLAN T23.2: SGLang states its KV pool (max_total_num_tokens), the tokens its
// running requests hold, and its retractions — its preemption. It counts no
// evictions, so that stays absent.
func TestLoadReadsTheKVPoolSGLangBuilt(t *testing.T) {
	body := "sglang:token_usage{model_name=\"m\"} 0.5\n" +
		"sglang:max_total_num_tokens{model_name=\"m\"} 120000\n" +
		"sglang:kv_used_tokens{model_name=\"m\"} 60000\n" +
		"sglang:num_retracted_requests_total{model_name=\"m\"} 7\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			_, _ = w.Write([]byte(body))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	ld, err := New(srv.URL, "").Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ld.KVCapacityTokens == nil || *ld.KVCapacityTokens != 120000 {
		t.Fatalf("pool: %v", ld.KVCapacityTokens)
	}
	if ld.KVUsedTokens == nil || *ld.KVUsedTokens != 60000 {
		t.Fatalf("used: %v", ld.KVUsedTokens)
	}
	if ld.PreemptionsTotal == nil || *ld.PreemptionsTotal != 7 {
		t.Fatalf("retractions: %v", ld.PreemptionsTotal)
	}
	if ld.EvictionsTotal != nil {
		t.Fatalf("SGLang counts no evictions: %v", ld.EvictionsTotal)
	}
}

// A release whose /metrics lacks the pool still states it in /server_info,
// which the driver reads once with the slot count; with neither, no pool.
func TestTheKVPoolFallsBackToServerInfo(t *testing.T) {
	info := `{"max_total_num_tokens":90000,"internal_states":[{"effective_max_running_requests_per_dp":48}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/metrics":
			_, _ = w.Write([]byte("sglang:token_usage 0.1\n"))
		case "/server_info":
			_, _ = w.Write([]byte(info))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	d := New(srv.URL, "")
	if ld, _ := d.Load(context.Background()); ld.KVCapacityTokens != nil {
		t.Fatalf("before /server_info is read nothing is stated: %v", *ld.KVCapacityTokens)
	}
	if n, ok := d.Slots(context.Background()); !ok || n != 48 {
		t.Fatalf("slots: %d %v", n, ok)
	}
	ld, err := d.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ld.KVCapacityTokens == nil || *ld.KVCapacityTokens != 90000 {
		t.Fatalf("pool from /server_info: %v", ld.KVCapacityTokens)
	}
	if ld.KVUsedTokens != nil || ld.PreemptionsTotal != nil {
		t.Fatalf("not on this /metrics, not stated: %v %v", ld.KVUsedTokens, ld.PreemptionsTotal)
	}
}
