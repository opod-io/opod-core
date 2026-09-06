package api

import (
	"errors"
	"net/http"
	"testing"

	"github.com/opod-io/opod/internal/engines"
)

// TestUpstreamPassthrough: a remote/OpenAI-compatible engine's 503/429/4xx is
// relayed with its own status and message; 401/403/5xx and transport errors
// stay the gateway's 502.
func TestUpstreamPassthrough(t *testing.T) {
	st, code, msg, ok := upstreamPassthrough(engines.Upstream("vllm", "chat", 503, []byte(`{"error":{"message":"no workers are awake — waking","type":"invalid_request"}}`)))
	if !ok || st != http.StatusServiceUnavailable || code != "invalid_request" || msg != "vllm: no workers are awake — waking" {
		t.Fatalf("503 passthrough: %d %s %q %v", st, code, msg, ok)
	}
	if st, code, _, ok := upstreamPassthrough(engines.Upstream("vllm", "chat", 429, []byte("slow down"))); !ok || st != 429 || code != "upstream_429" {
		t.Fatalf("429 passthrough: %d %s %v", st, code, ok)
	}
	for _, status := range []int{401, 403, 500, 502} {
		if _, _, _, ok := upstreamPassthrough(engines.Upstream("vllm", "chat", status, nil)); ok {
			t.Fatalf("%d must stay a 502 (ours to fix)", status)
		}
	}
	if _, _, _, ok := upstreamPassthrough(engines.Unreachable("vllm", "http://x", errors.New("dial"))); ok {
		t.Fatal("transport errors are not passthrough")
	}
}
