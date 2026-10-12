package agent

// A worker whose own engine is down — the process was killed or is restarting,
// so the worker's dial to it is refused — answered a plain-text 502, which the
// leader read as the engine's ERROR: shown to the caller, never asked of a
// sibling, with a hint to start the leader's own llama-server (design-partner
// cell, PLAN T8.18: 20 of 20 queued requests after an engine kill). The answer
// is now typed, and the leader's client reads it back as "nothing answered".

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/engines"
)

// deadEngineURL is an address nothing listens on: a dial is refused.
func deadEngineURL(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return "http://" + addr
}

func TestAWorkerWhoseEngineIsDownAnswersNothingAnswered(t *testing.T) {
	worker := &Server{Engine: engines.MustNew("vllm", deadEngineURL(t), "")}
	srv := httptest.NewServer(http.HandlerFunc(worker.chatCompletions))
	t.Cleanup(srv.Close)

	// The leader's side of the hop: the same client the router dials a worker with.
	leaderSide := engines.MustNew("vllm", srv.URL, "")
	for _, stream := range []bool{true, false} {
		_, err := leaderSide.Chat(context.Background(), engines.ChatRequest{
			Model: "m", Stream: stream, Messages: []engines.Message{{Role: "user", Content: "hi"}}})
		if !errors.Is(err, engines.ErrUnreachable) {
			t.Fatalf("stream=%v: want the worker's dead engine read as unreachable, got %v", stream, err)
		}
		if errors.Is(err, engines.ErrUpstream) {
			t.Fatalf("stream=%v: a dead engine is not an engine's answer: %v", stream, err)
		}
		if !strings.Contains(err.Error(), "connection refused") {
			t.Fatalf("stream=%v: the worker's own words must survive the hop: %v", stream, err)
		}
	}

	// What a leader older than the type sees: still a 502.
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, want 502 for every leader", resp.StatusCode)
	}
}

// An engine that answered with an error of its own is still that error.
func TestAWorkerRelaysAnEngineErrorAsAnEngineError(t *testing.T) {
	eng := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"boom","type":"server_error"}}`, http.StatusInternalServerError)
	}))
	t.Cleanup(eng.Close)
	worker := &Server{Engine: engines.MustNew("vllm", eng.URL, "")}
	srv := httptest.NewServer(http.HandlerFunc(worker.chatCompletions))
	t.Cleanup(srv.Close)

	_, err := engines.MustNew("vllm", srv.URL, "").Chat(context.Background(), engines.ChatRequest{Model: "m", Stream: true})
	if !errors.Is(err, engines.ErrUpstream) || errors.Is(err, engines.ErrUnreachable) {
		t.Fatalf("an engine's own error must stay an engine error, got %v", err)
	}
}
