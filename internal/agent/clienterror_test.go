package agent

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/engines"
)

// The worker answers its engine's refusal of the request with the same status,
// so the leader relays it as the caller's error (engines.ClientStatus).
func TestAWorkerRelaysItsEnginesRefusalOfTheRequest(t *testing.T) {
	rec := httptest.NewRecorder()
	writeEngineFailure(rec, engines.Upstream("llamacpp", "chat", 400, []byte(`{"error":{"message":"request (7739 tokens) exceeds the available context size (2048 tokens)"}}`)))
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "exceeds the available context size") {
		t.Fatalf("a refusal of the request is relayed as one, in the engine's own words: %d %s", rec.Code, rec.Body.String())
	}
	// …and the leader reads it back as the engine's 400, which it relays.
	back := engines.FromWorkerAnswer("llamacpp", "http://w:8081", "chat", rec.Code, rec.Body.Bytes())
	if status, ok := engines.ClientStatus(back); !ok || status != 400 {
		t.Fatalf("the leader must read the worker's answer as the caller's 400: %v", back)
	}
	rec = httptest.NewRecorder()
	writeEngineFailure(rec, engines.Upstream("llamacpp", "chat", 500, []byte(`boom`)))
	if rec.Code != 502 {
		t.Fatalf("the engine's own failure stays 502: %d", rec.Code)
	}
}
