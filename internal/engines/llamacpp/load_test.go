package llamacpp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// shippedMetrics is EVERY series the llama-server in the image we ship emits
// (read from inside a serving worker, 2026-09-28: opod-worker-llamacpp-nvidia
// at the digest the chart pins) — and `kv_cache_usage_ratio` is not among
// them. A fixture that supplied it let the product read zero for four days
// (PLAN T6.35). If a name core reads leaves this list, this test is where it
// shows.
const shippedMetrics = `# TYPE llamacpp:n_busy_slots_per_decode gauge
llamacpp:n_busy_slots_per_decode 2
llamacpp:n_decode_total 10
llamacpp:n_tokens_max 4096
llamacpp:predicted_tokens_seconds 30
llamacpp:prompt_seconds_total 5
llamacpp:prompt_tokens_cached_total 0
llamacpp:prompt_tokens_seconds 100
llamacpp:prompt_tokens_total 500
llamacpp:requests_deferred 1
llamacpp:requests_processing 2
llamacpp:spec_decode_num_accepted_tokens_total 0
llamacpp:spec_decode_num_draft_tokens_total 0
llamacpp:spec_decode_num_drafts_total 0
llamacpp:tokens_predicted_seconds_total 20
llamacpp:tokens_predicted_total 4242
`

func server(t *testing.T, metrics string, slots int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/metrics":
			_, _ = w.Write([]byte(metrics))
		case "/props":
			if slots <= 0 {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(`{"default_generation_settings":{},"total_slots":` + itoa(slots) + `,"chat_template":""}`))
		default:
			http.NotFound(w, r)
		}
	}))
}

func itoa(n int) string { return strings.TrimSpace(strings.Repeat(" ", 0) + string(rune('0'+n))) }

// Against the metrics the shipped image actually emits, KV pressure is slot
// occupancy: 2 of 4 slots busy reads 50 %.
func TestLoadDerivesKVPressureFromSlotOccupancy(t *testing.T) {
	srv := server(t, shippedMetrics, 4)
	defer srv.Close()
	d := New(srv.URL)
	ld, err := d.Load(context.Background())
	if err != nil || ld.KVUsedPct != 50 || ld.QueueDepth != 1 || ld.PrefixHitPct != 0 {
		t.Fatalf("sample %+v %v", ld, err)
	}
	if d.slots != 4 {
		t.Fatalf("the slot count is kept: %d", d.slots)
	}
	// A build that emits the ratio is read as before, and the ratio wins.
	withRatio := server(t, "llamacpp:kv_cache_usage_ratio 0.25\n"+shippedMetrics, 4)
	defer withRatio.Close()
	if ld, _ := New(withRatio.URL).Load(context.Background()); ld.KVUsedPct != 25 {
		t.Fatalf("a reported ratio wins: %+v", ld)
	}
	// No /props: nothing to divide by, so the field stays 0 — never a fake number.
	noProps := server(t, shippedMetrics, 0)
	defer noProps.Close()
	if ld, _ := New(noProps.URL).Load(context.Background()); ld.KVUsedPct != 0 {
		t.Fatalf("without a slot count the pressure is unknown, not invented: %+v", ld)
	}
	// a server without --metrics answers 404 → an upstream error, never a fake zero sample
	off := httptest.NewServer(http.NotFoundHandler())
	defer off.Close()
	if _, err := New(off.URL).Load(context.Background()); err == nil {
		t.Fatal("404 /metrics must be an error")
	}
}
