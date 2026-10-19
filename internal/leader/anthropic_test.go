package leader

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The leader's own answers on /v1/messages are Anthropic-shaped too (T19.3): a
// refusal of the request, and the admission gate's 503 when nothing can serve
// the model — which keeps its Retry-After, the autoscaler's wake signal.
func TestAnthropicRoutesAnswerInAnthropicShape(t *testing.T) {
	srv := drainedLeader(t) // m's only worker is drained: nothing can serve it
	srv.applyPolicySnapshot(&PolicySnapshot{Revision: "p0"})
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	post := func(path, body string) (*http.Response, map[string]any) {
		t.Helper()
		resp, err := http.Post(ts.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp, out
	}
	errType := func(out map[string]any) string {
		e, _ := out["error"].(map[string]any)
		if out["type"] != "error" || e == nil {
			return ""
		}
		s, _ := e["type"].(string)
		return s
	}

	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		resp, out := post(path, `{"model":"m","max_tokens":8,"thinking":{"type":"enabled","budget_tokens":64},"messages":[{"role":"user","content":"hi"}]}`)
		if resp.StatusCode != http.StatusBadRequest || errType(out) != "invalid_request_error" {
			t.Errorf("%s with thinking: %d %v", path, resp.StatusCode, out)
		}
		resp, out = post(path, `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
		if resp.StatusCode != http.StatusServiceUnavailable || errType(out) != "overloaded_error" || resp.Header.Get("Retry-After") == "" {
			t.Errorf("%s with nothing to serve m: %d %v Retry-After %q", path, resp.StatusCode, out, resp.Header.Get("Retry-After"))
		}
	}
}
