package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/engines"
)

// answerEngine streams one delta and finishes at once: an engine with no wait
// of its own, so whatever the clock reads came from before it was called.
type answerEngine struct{ cancelEngine }

func (e *answerEngine) Chat(context.Context, engines.ChatRequest) (<-chan engines.StreamEvent, error) {
	out := make(chan engines.StreamEvent, 2)
	out <- engines.StreamEvent{Delta: "hi"}
	out <- engines.StreamEvent{Done: true, Usage: &engines.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}}
	close(out)
	return out, nil
}

// The time to first token is measured from the request's ARRIVAL, which the
// leader stamps before its admission gate (ADR-091): a request held 150 ms for
// a worker slot reports at least 150 ms, not the engine's few. Measured from
// after the gate, every leader TTFT on the design-partner cell read ≤ 0.25 s
// while callers waited ~4.4 s (2026-10-07, PLAN T18.1 re-run).
func TestTTFTAndLatencyIncludeTheWaitBeforeTheHandler(t *testing.T) {
	st := usageTestStore(t)
	var seen time.Duration
	h := &Handler{Engine: &answerEngine{}, Store: st, Default: "m",
		OnTTFT: func(_ context.Context, d time.Duration) { seen = d }}

	const waited = 150 * time.Millisecond
	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req = req.WithContext(WithArrival(req.Context(), time.Now().Add(-waited)))
	rec := httptest.NewRecorder()
	h.ChatCompletions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}

	row := waitForUsage(t, st)
	if row.Outcome != "ok" {
		t.Fatalf("outcome %q, want ok", row.Outcome)
	}
	if row.TTFTMS < int(waited.Milliseconds()) {
		t.Fatalf("usage ttft_ms = %d, want ≥ %d: the wait before the handler is part of it", row.TTFTMS, waited.Milliseconds())
	}
	if row.LatencyMS < row.TTFTMS {
		t.Fatalf("latency_ms %d < ttft_ms %d: both are measured from the same arrival", row.LatencyMS, row.TTFTMS)
	}
	if seen < waited {
		t.Fatalf("the /loadz window saw %s, want ≥ %s", seen, waited)
	}
}

// A handler nobody stamped (no leader dispatch in front of it) measures from
// when it starts, as it always did.
func TestTTFTWithoutAnArrivalStartsAtTheHandler(t *testing.T) {
	if d := time.Since(arrivalOf(context.Background())); d < 0 || d > time.Second {
		t.Fatalf("an unstamped request's arrival is now, got %s ago", d)
	}
	at := time.Now().Add(-time.Minute)
	if got := arrivalOf(WithArrival(context.Background(), at)); !got.Equal(at) {
		t.Fatalf("the stamp is read back: got %s, want %s", got, at)
	}
}
