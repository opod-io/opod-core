package openaicompat

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/engines"
)

// A worker whose pod was just removed is not a closed port — its address is
// gone, packets are dropped, and a connect HANGS. This client had no connect
// timeout of its own, so it inherited the 30 s of Go's default transport: a
// request routed to the removed worker stalled for 20–30 s before the router's
// "ask the next worker" could run (measured on a cluster at every pod swap; one
// request in each was answered 502 by an outer deadline first). Responses
// stream, so there is no overall deadline — but a connect that has not happened
// in a few seconds is not going to.
func TestAConnectThatHangsFailsFastAsUnreachable(t *testing.T) {
	c := NewClient("vllm", "http://192.0.2.1:8081", nil) // TEST-NET-1: never answers, never refuses
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := c.Chat(ctx, engines.ChatRequest{Model: "m", Messages: []engines.Message{{Role: "user", Content: "hi"}}})
	took := time.Since(start)
	if !errors.Is(err, engines.ErrUnreachable) {
		t.Fatalf("want an unreachable error, got %v after %s", err, took)
	}
	if took > ConnectTimeout+2*time.Second {
		t.Fatalf("the connect took %s to give up; the router's next-worker rule waits behind it (want ≤ %s)", took, ConnectTimeout)
	}
}
