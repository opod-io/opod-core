package router

// The leader's REQUEST PATH to a worker authenticates as the node.
//
// Every leader→worker call in the scheduler signs (auth.SignRequest) and sends
// the bearer header only for the transition. The router's client did not: it
// put the worker token in an Authorization header and nothing else, so a worker
// configured HMAC-only (OPOD_REJECT_BEARER=1) answered
// `401 unauthorized (HMAC required; bearer disabled)` to every completion —
// found on the design-partner cell on 2026-09-21, after the same switch had
// already been found to break the worker's model load (core T10.7).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/auth"
	_ "github.com/opod-io/opod/internal/engines/all"
)

func TestTheRouterSignsItsCallsToAWorker(t *testing.T) {
	// Not an `sk-orc-…` literal on purpose: the push scan looks for that shape, and
	// a tripwire that learns to ignore test fixtures stops being a tripwire.
	const nodeID, token = "n_worker1", "worker-token-for-test"
	var gotHeader, gotBearer string
	// A worker that accepts ONLY the signature, the way OPOD_REJECT_BEARER=1
	// makes it.
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get(auth.HMACHeader)
		gotBearer = r.Header.Get("Authorization")
		if _, err := auth.VerifyRequest(r, func(string) (string, error) { return token, nil }); err != nil {
			http.Error(w, "unauthorized (HMAC required; bearer disabled): "+err.Error(), http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m"}]}`))
	}))
	defer worker.Close()

	rt := New(nil, nil)
	eng := rt.getOrCreateRemote(nodeID, strings.TrimPrefix(worker.URL, "http://"), token)
	if err := eng.Health(context.Background()); err != nil {
		t.Fatalf("a signed call to an HMAC-only worker must be accepted: %v", err)
	}
	if gotHeader == "" {
		t.Error("the router sent no " + auth.HMACHeader + " header — an HMAC-only worker refuses every request")
	}
	if !strings.HasPrefix(gotBearer, "Bearer ") {
		t.Errorf("the transition bearer header is still sent for one release, got %q", gotBearer)
	}
}
