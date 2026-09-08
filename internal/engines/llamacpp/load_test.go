package llamacpp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLoadScrapesLlamaServerMetrics(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("# TYPE llamacpp:kv_cache_usage_ratio gauge\nllamacpp:kv_cache_usage_ratio 0.5\nllamacpp:requests_deferred 1\nllamacpp:tokens_predicted_total 4242\n"))
	}))
	defer srv.Close()
	ld, err := New(srv.URL).Load(context.Background())
	if err != nil || ld.KVUsedPct != 50 || ld.QueueDepth != 1 || ld.PrefixHitPct != 0 {
		t.Fatalf("sample %+v %v", ld, err)
	}
	// a server without --metrics answers 404 → an upstream error, never a fake zero sample
	off := httptest.NewServer(http.NotFoundHandler())
	defer off.Close()
	if _, err := New(off.URL).Load(context.Background()); err == nil {
		t.Fatal("404 /metrics must be an error")
	}
}
