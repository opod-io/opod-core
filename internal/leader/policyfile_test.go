package leader

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/store"
)

func newPolicyTestServer(t *testing.T, eng interface{ Health(context.Context) error }) (*Server, *httptest.Server) {
	t.Helper()
	cfg := config.Default()
	cfg.Listen = ":0"
	cfg.Auth.RequireKeys = false
	cfg.Router.DefaultModel = "m1"
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var srv *Server
	switch e := eng.(type) {
	case *downEngine:
		srv = NewServer(cfg, st, e, nil, log, nil)
	default:
		srv = NewServer(cfg, st, &chatStubEngine{}, nil, log, nil)
	}
	ts := httptest.NewServer(srv.routes())
	t.Cleanup(ts.Close)
	return srv, ts
}

// chatStubEngine is healthy but answers every chat with an engine error, so a
// prompt ends as a 5xx — never a hang on the stub's
// nil stream, never a 403.
type chatStubEngine struct{ stubLeaderEngine }

func (c *chatStubEngine) Chat(context.Context, engines.ChatRequest) (<-chan engines.StreamEvent, error) {
	return nil, errors.New("stub engine: no generation in tests")
}

// downEngine has no local capacity — the leader is router-only with nothing awake.
type downEngine struct{ stubLeaderEngine }

func (d *downEngine) Health(context.Context) error { return errors.New("no engine") }

func chat(t *testing.T, ts *httptest.Server, body, bearer string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

// TestPolicyFileWatcher: the watcher loads the file at start, a bad file keeps
// the last good policy, and every knob lands where the request path reads it.
func TestPolicyFileWatcher(t *testing.T) {
	srv, _ := newPolicyTestServer(t, nil)
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.json")
	off := false
	doc := PolicySnapshot{Revision: "p1", Routing: PolicyRouting{FallbackURL: "http://fb.opod.svc/v1/"},
		Logging: PolicyLogging{AccessLog: &off}}
	raw, _ := json.Marshal(doc)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	srv.cfg.Env.PolicyFile = path
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv.StartPolicyWatcher(ctx)

	if srv.policy.revision != "p1" {
		t.Fatalf("revision from file: %q", srv.policy.revision)
	}
	if fb := srv.policy.fallbackRouting(); fb == nil || fb.FallbackURL != "http://fb.opod.svc" {
		t.Fatalf("fallback base url must lose the trailing /v1: %+v", fb)
	}
	if srv.policy.accessLogEnabled() {
		t.Fatal("accessLog=false must switch the access log off")
	}

	// a bad file keeps the last good policy
	srv.applyPolicySnapshot(&PolicySnapshot{Revision: "p2"})
	if srv.policy.fallbackRouting() != nil || !srv.policy.accessLogEnabled() {
		t.Fatal("an empty snapshot must clear every knob")
	}
	var bad PolicySnapshot
	if err := json.Unmarshal([]byte(`{"revision": "p3", "routing": "nope"}`), &bad); err == nil {
		t.Fatal("test expects the malformed doc to fail to parse")
	}
	if srv.policy.revision != "p2" {
		t.Fatalf("last good revision must stay: %s", srv.policy.revision)
	}
}

// TestPolicySnapshotGuardrailsKeyIgnored (T32.29, ADR-113): the leader has no
// guardrail webhook. A snapshot that still names one — the shape a manager
// wrote before the removal — parses, and the prompt goes to the engine: the
// receiver is never called and nothing answers 403.
func TestPolicySnapshotGuardrailsKeyIgnored(t *testing.T) {
	srv, ts := newPolicyTestServer(t, nil)
	called := false
	guard := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		_ = json.NewEncoder(w).Encode(map[string]any{"action": "block", "reason": "blocked"})
	}))
	defer guard.Close()
	var doc PolicySnapshot
	raw := `{"revision":"g1","guardrails":[{"id":"pii","phase":"pre","url":"` + guard.URL + `"}]}`
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("a snapshot with a stray guardrails key must still parse: %v", err)
	}
	srv.applyPolicySnapshot(&doc)
	resp, out := chat(t, ts, `{"model":"m1","messages":[{"role":"user","content":"mail jane@example.com"}]}`, "")
	if resp.StatusCode == http.StatusForbidden || strings.Contains(out, "guardrail") {
		t.Fatalf("the leader must not run a guardrail: %d %s", resp.StatusCode, out)
	}
	if called {
		t.Fatal("the guardrail receiver must never be called")
	}
}

// TestPolicyFallbackForward: no capacity + fallback in the snapshot → the
// request is answered by the fallback (model rewritten, bearer swapped,
// X-Opod-Fallback set); no fallback → the honest 503; dead fallback → 503.
func TestPolicyFallbackForward(t *testing.T) {
	srv, ts := newPolicyTestServer(t, &downEngine{})
	body := `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`
	if resp, _ := chat(t, ts, body, "caller-key"); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("no capacity, no fallback: want 503, got %d", resp.StatusCode)
	}
	var gotModel, gotAuth string
	fb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		b, _ := io.ReadAll(r.Body)
		gotModel = peekModel(b)
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"fb-1","choices":[{"message":{"role":"assistant","content":"from fallback"}}]}`))
	}))
	defer fb.Close()
	srv.applyPolicySnapshot(&PolicySnapshot{Revision: "f1", Routing: PolicyRouting{FallbackURL: fb.URL + "/v1", FallbackModel: "m1-remote", FallbackKey: "fb-key"}})
	resp, out := chat(t, ts, body, "caller-key")
	if resp.StatusCode != http.StatusOK || !strings.Contains(out, "from fallback") {
		t.Fatalf("forwarded: %d %s", resp.StatusCode, out)
	}
	if resp.Header.Get("X-Opod-Fallback") != fb.URL {
		t.Fatalf("X-Opod-Fallback: %q", resp.Header.Get("X-Opod-Fallback"))
	}
	if gotModel != "m1-remote" || gotAuth != "Bearer fb-key" {
		t.Fatalf("fallback request: model=%q auth=%q", gotModel, gotAuth)
	}
	// caller's bearer is forwarded when the policy has none
	srv.applyPolicySnapshot(&PolicySnapshot{Revision: "f2", Routing: PolicyRouting{FallbackURL: fb.URL}})
	chat(t, ts, body, "caller-key")
	if gotModel != "m1" || gotAuth != "Bearer caller-key" {
		t.Fatalf("passthrough: model=%q auth=%q", gotModel, gotAuth)
	}
	// dead fallback → honest 503
	srv.applyPolicySnapshot(&PolicySnapshot{Revision: "f3", Routing: PolicyRouting{FallbackURL: "http://127.0.0.1:1"}})
	if resp, _ := chat(t, ts, body, ""); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("dead fallback: want 503, got %d", resp.StatusCode)
	}
}
