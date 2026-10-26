package sglang

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The engine's own TTFT median (T24.25) comes from its histogram: nothing on
// the first scrape (no window yet), the median of what arrived since on the
// next, and nothing from a server that publishes no histogram.
func TestLoadReadsTheEnginesOwnTTFT(t *testing.T) {
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fast := 4 * n // every scrape adds 4 requests answered in under 20 ms
		n++
		const h = "sglang:time_to_first_token_seconds"
		_, _ = fmt.Fprintf(w, "# TYPE %[1]s histogram\n%[1]s_bucket{le=\"0.02\",model_name=\"m\"} %[2]d\n%[1]s_bucket{le=\"0.04\",model_name=\"m\"} %[2]d\n%[1]s_bucket{le=\"+Inf\",model_name=\"m\"} %[2]d\n", h, fast)
	}))
	defer srv.Close()
	d := New(srv.URL, "")
	ld, err := d.Load(context.Background())
	if err != nil || ld.EngineTTFTP50Ms != nil {
		t.Fatalf("first scrape: %+v %v", ld.EngineTTFTP50Ms, err)
	}
	ld, err = d.Load(context.Background())
	if err != nil || ld.EngineTTFTP50Ms == nil || *ld.EngineTTFTP50Ms != 10 {
		t.Fatalf("second scrape: want 10 ms, got %v (%v)", ld.EngineTTFTP50Ms, err)
	}

	bare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("sglang:num_requests_waiting{model_name=\"m\"} 0\n"))
	}))
	defer bare.Close()
	b := New(bare.URL, "")
	_, _ = b.Load(context.Background())
	if ld, _ := b.Load(context.Background()); ld.EngineTTFTP50Ms != nil {
		t.Fatalf("no histogram must say nothing, got %d", *ld.EngineTTFTP50Ms)
	}
}
