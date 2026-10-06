package leader

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/opod-io/opod-sdk/adminapi"
	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/store"
)

// Nearest-rank percentiles over known samples: 1..100 ms → p50 50, p95 95.
func TestTTFTWindowPercentiles(t *testing.T) {
	var w ttftWindow
	now := time.Now()
	if p50, p95 := w.percentiles(now); p50 != 0 || p95 != 0 {
		t.Fatalf("empty window = %d/%d, want 0/0 (not measured)", p50, p95)
	}
	for i := 100; i >= 1; i-- { // order of arrival must not matter
		w.add(now, time.Duration(i)*time.Millisecond)
	}
	if p50, p95 := w.percentiles(now); p50 != 50 || p95 != 95 {
		t.Fatalf("p50/p95 = %d/%d, want 50/95", p50, p95)
	}
	// A zero is not a sample; a sub-millisecond one is 1 ms, never "instant".
	var z ttftWindow
	z.add(now, 0)
	z.add(now, 300*time.Microsecond)
	if p50, _ := z.percentiles(now); p50 != 1 {
		t.Fatalf("sub-ms sample = %d, want 1", p50)
	}
}

// Samples older than the span leave the window.
func TestTTFTWindowForgetsTheOldMinute(t *testing.T) {
	var w ttftWindow
	now := time.Now()
	w.add(now.Add(-2*time.Minute), 5*time.Second)
	w.add(now.Add(-10*time.Second), 200*time.Millisecond)
	if p50, p95 := w.percentiles(now); p50 != 200 || p95 != 200 {
		t.Fatalf("p50/p95 = %d/%d, want 200/200: an old sample counted", p50, p95)
	}
	if p50, _ := w.percentiles(now.Add(2 * time.Minute)); p50 != 0 {
		t.Fatalf("a window with nothing recent reported %d", p50)
	}
}

// Memory is fixed: past the cap the oldest samples are overwritten.
func TestTTFTWindowIsBounded(t *testing.T) {
	var w ttftWindow
	now := time.Now()
	for i := 0; i < ttftWindowCap; i++ {
		w.add(now, 10*time.Second)
	}
	for i := 0; i < ttftWindowCap; i++ {
		w.add(now, 100*time.Millisecond)
	}
	if w.n != ttftWindowCap {
		t.Fatalf("n = %d, want the cap %d", w.n, ttftWindowCap)
	}
	if _, p95 := w.percentiles(now); p95 != 100 {
		t.Fatalf("p95 = %d, want 100: the overwritten samples still count", p95)
	}
}

// /loadz carries the window, and a probe's first token does not move it.
func TestLoadzCarriesTTFT(t *testing.T) {
	cfg := config.Default()
	cfg.Listen = ":0"
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := NewServer(cfg, st, &deadEngine{&stubLeaderEngine{}}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if srv.openaiH.OnTTFT == nil {
		t.Fatal("the gateway handler does not feed the TTFT window")
	}
	ctx := context.Background()
	for i := 1; i <= 20; i++ {
		srv.observeTTFT(ctx, time.Duration(i*10)*time.Millisecond)
	}
	srv.observeTTFT(context.WithValue(ctx, probeCtxKey{}, true), time.Hour)

	rec := httptest.NewRecorder()
	srv.loadz(rec, httptest.NewRequest(http.MethodGet, "/loadz", nil))
	var ld adminapi.Load
	if err := json.Unmarshal(rec.Body.Bytes(), &ld); err != nil {
		t.Fatal(err)
	}
	if ld.TTFTP50Ms != 100 || ld.TTFTP95Ms != 190 {
		t.Fatalf("ttft p50/p95 = %d/%d, want 100/190 (and the probe's hour ignored)", ld.TTFTP50Ms, ld.TTFTP95Ms)
	}
}

// trackLoad marks a probe on its context, which is how the TTFT window knows
// to skip it — the probe is served, and counted nowhere.
func TestTrackLoadMarksAProbe(t *testing.T) {
	srv := &Server{}
	var marked bool
	h := srv.trackLoad(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		marked, _ = r.Context().Value(probeCtxKey{}).(bool)
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set(ProbeHeader, "1")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if !marked {
		t.Fatal("a probe reached the handler unmarked: its TTFT would count")
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if marked {
		t.Fatal("an ordinary request was marked as a probe")
	}
}
