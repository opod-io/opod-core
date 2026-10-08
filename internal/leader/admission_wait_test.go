package leader

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// admissionWait is the opod_admission_wait_seconds histogram of one class:
// how many admitted requests it has seen and their summed wait.
func admissionWait(t *testing.T, class string) (count uint64, sum float64) {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "opod_admission_wait_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "class" && l.GetValue() == class {
					return m.GetHistogram().GetSampleCount(), m.GetHistogram().GetSampleSum()
				}
			}
		}
	}
	return 0, 0
}

// Every admitted request records its wait for a slot (ADR-091): 0 for one
// that found a slot free, the hold for one that waited. Shed requests are not
// admitted and are not in it.
func TestAdmissionWaitIsRecordedPerAdmittedRequest(t *testing.T) {
	srv := slottedLeader(t, 1, PolicyAdmission{HoldMs: 5000})
	n0, s0 := admissionWait(t, "standard")

	busy := admitNow(t, srv, classStandard, "a") // a free slot: waits 0
	out := make(chan admitted, 1)
	admitAs(srv, context.Background(), "held", classStandard, "a", out)
	waitHeld(t, srv, 1)
	const held = 120 * time.Millisecond
	time.Sleep(held)
	busy()
	a := next(t, out)
	if a.why != "" {
		t.Fatalf("the held request is served when the slot frees, got %q", a.why)
	}
	a.release()

	n1, s1 := admissionWait(t, "standard")
	if n1-n0 != 2 {
		t.Fatalf("two admitted requests, the histogram saw %d", n1-n0)
	}
	if got := s1 - s0; got < held.Seconds() {
		t.Fatalf("the waits sum to %.3fs, want ≥ %.3fs (the held one's hold)", got, held.Seconds())
	}
}

// The leader stamps a request's arrival BEFORE its admission gate, so what the
// handler records for it — the usage row's latency, and on a streamed answer
// its time to first token — includes the time it waited for a slot. Measured
// from after the gate, every leader TTFT on the design-partner cell read
// ≤ 0.25 s while callers waited ~4.4 s for their first token (2026-10-07).
func TestDispatchMeasuresFromArrivalBeforeTheGate(t *testing.T) {
	srv := slottedLeader(t, 1, PolicyAdmission{HoldMs: 5000})
	busy := admitNow(t, srv, classStandard, "a")

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			bytes.NewReader([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)))
		req.Header.Set("Content-Type", "application/json")
		srv.dispatchOpenAIChat(rec, req)
		done <- rec
	}()
	waitHeld(t, srv, 1)
	const held = 150 * time.Millisecond
	time.Sleep(held)
	busy() // the slot frees; the request goes on to its (unreachable) worker
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the request never finished")
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		rows, err := srv.store.Usage().Recent(context.Background(), 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) > 0 {
			if rows[0].LatencyMS < int(held.Milliseconds()) {
				t.Fatalf("usage latency_ms = %d, want ≥ %d: the wait for a slot is part of what the caller saw", rows[0].LatencyMS, held.Milliseconds())
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no usage row for the request")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
