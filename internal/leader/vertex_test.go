package leader

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// T19.4: with OPOD_VERTEX=1 the leader answers Vertex's predict and health
// routes without a key; what it cannot serve is refused by name, and an
// instance it cannot place fails the request with that instance named.
func TestVertexRoutes(t *testing.T) {
	srv := drainedLeader(t) // m's only worker is drained: nothing can serve it
	srv.applyPolicySnapshot(&PolicySnapshot{Revision: "p0"})
	srv.cfg.Auth.RequireKeys = true
	post := func(ts *httptest.Server, path, body string) (*http.Response, string) {
		t.Helper()
		resp, err := http.Post(ts.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}

	off := httptest.NewServer(srv.routes())
	// Off, the path is just an unknown /v1 path: the key check answers first.
	if resp, _ := post(off, "/v1/vertex:predict", `{"instances":[]}`); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("vertex off: %d, want the /v1 key refusal", resp.StatusCode)
	}
	off.Close()

	srv.cfg.Env.Vertex = true
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()
	msgs := `"messages":[{"role":"user","content":"hi"}]`
	for name, body := range map[string]string{
		"gemini":       `{"contents":[{"parts":[{"text":"hi"}]}]}`,
		"no instances": `{"instances":[]}`,
		"not an array": `{"instances":{"a":1}}`,
		"no messages":  `{"instances":[{"model":"m"}]}`,
		"streams":      `{"instances":[{"model":"m","stream":true,` + msgs + `}]}`,
		"too many":     `{"instances":[` + strings.TrimSuffix(strings.Repeat(`{"model":"m",`+msgs+`},`, 17), ",") + `]}`,
	} {
		resp, b := post(ts, "/v1/vertex:predict", body)
		var e struct {
			Error struct {
				Code int `json:"code"`
			} `json:"error"`
		}
		if resp.StatusCode != http.StatusBadRequest || json.Unmarshal([]byte(b), &e) != nil || e.Error.Code != 400 {
			t.Errorf("%s: %d %s", name, resp.StatusCode, b)
		}
	}
	// No key, keys required: the predict route is Vertex-authenticated, so it
	// reaches admission — which has nothing to give m, and names instance 1.
	resp, b := post(ts, "/v1/vertex:predict", `{"instances":[{"model":"m",`+msgs+`},{"model":"m",`+msgs+`}],"parameters":{"max_tokens":8}}`)
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" || resp.Header.Get("X-Opod-Vertex-Instance") == "" {
		t.Fatalf("predict with no capacity: %d %v %s", resp.StatusCode, resp.Header, b)
	}
	// rawPredict: a chat body passed through.
	if resp, b := post(ts, "/v1/vertex:predict", `{"model":"m",`+msgs+`}`); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("rawPredict with no capacity: %d %s", resp.StatusCode, b)
	}
	hr, err := http.Get(ts.URL + "/v1/vertex/health")
	if err != nil {
		t.Fatal(err)
	}
	hr.Body.Close()
	if hr.StatusCode == http.StatusNotFound || hr.StatusCode == http.StatusUnauthorized {
		t.Fatalf("health: %d", hr.StatusCode)
	}
	// The chat route keeps its key.
	if resp, _ := post(ts, "/v1/chat/completions", `{"model":"m",`+msgs+`}`); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("chat without a key: %d, want 401", resp.StatusCode)
	}
}
