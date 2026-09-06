package controlplane

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

	"github.com/opod-io/opod/internal/api"
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
	t.Cleanup(func() { api.SetGuardrails(nil) })
	return srv, ts
}

// chatStubEngine is healthy but answers every chat with an engine error, so a
// prompt that passes the guardrails ends as a 5xx — never a hang on the stub's
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
		Logging: PolicyLogging{AccessLog: &off},
		Guardrails: []GuardrailRule{
			{ID: "pii", Phase: "pre", URL: "http://guard.opod.svc/check"},
			{ID: "shadow", Phase: "logging_only", URL: "http://guard.opod.svc/observe"},
			{ID: "broken", Phase: "sideways", URL: "http://x"},
			{ID: "nourl", Phase: "pre"},
		}}
	raw, _ := json.Marshal(doc)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPOD_POLICY_FILE", path)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv.StartPolicyWatcher(ctx)

	reg := api.Guardrails()
	if reg == nil || len(reg.Pre.Guards()) != 1 || len(reg.LoggingOnly.Guards()) != 1 || !reg.Post.IsEmpty() {
		t.Fatalf("registry from file: %+v", reg)
	}
	if reg.Pre.Guards()[0].Name() != "pii" {
		t.Fatalf("rule id must name the guard: %q", reg.Pre.Guards()[0].Name())
	}
	if srv.policy.rules != 2 {
		t.Fatalf("2 valid rules expected, got %d", srv.policy.rules)
	}
	if fb := srv.policy.fallbackRouting(); fb == nil || fb.FallbackURL != "http://fb.opod.svc" {
		t.Fatalf("fallback base url must lose the trailing /v1: %+v", fb)
	}
	if srv.policy.accessLogEnabled() {
		t.Fatal("accessLog=false must switch the access log off")
	}

	// a bad file keeps the last good policy
	srv.applyPolicySnapshot(&PolicySnapshot{Revision: "p2"})
	if api.Guardrails() != nil || srv.policy.fallbackRouting() != nil || !srv.policy.accessLogEnabled() {
		t.Fatal("an empty snapshot must clear every knob")
	}
	var bad PolicySnapshot
	if err := json.Unmarshal([]byte(`{"revision": "p3", "guardrails": "nope"}`), &bad); err == nil {
		t.Fatal("test expects the malformed doc to fail to parse")
	}
	if srv.policy.revision != "p2" {
		t.Fatalf("last good revision must stay: %s", srv.policy.revision)
	}
}

// TestPolicyGuardrailBlocks: a pre-phase webhook from the snapshot blocks a
// prompt end to end with the OpenAI guardrail_blocked shape; a later snapshot
// without the rule lets the same prompt through to the engine.
func TestPolicyGuardrailBlocks(t *testing.T) {
	srv, ts := newPolicyTestServer(t, nil)
	var seen string
	guard := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen = string(b)
		if r.Header.Get("Authorization") != "Bearer g-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if strings.Contains(seen, "@example.com") {
			_ = json.NewEncoder(w).Encode(map[string]any{"action": "block", "reason": "email address in prompt"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"action": "allow"})
	}))
	defer guard.Close()

	srv.applyPolicySnapshot(&PolicySnapshot{Revision: "g1", Guardrails: []GuardrailRule{
		{ID: "pii", Phase: "pre", URL: guard.URL, AuthKey: "g-secret", TimeoutMs: 2000},
	}})
	body := `{"model":"m1","messages":[{"role":"user","content":"mail jane@example.com"}]}`
	resp, out := chat(t, ts, body, "")
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(out, "guardrail_blocked") || !strings.Contains(out, "email address in prompt") {
		t.Fatalf("blocked prompt: %d %s", resp.StatusCode, out)
	}
	if !strings.Contains(seen, "jane@example.com") {
		t.Fatalf("webhook must see the request body: %s", seen)
	}
	// fail-closed by default: an unreachable receiver blocks
	srv.applyPolicySnapshot(&PolicySnapshot{Revision: "g2", Guardrails: []GuardrailRule{
		{ID: "dead", Phase: "pre", URL: "http://127.0.0.1:1/never", TimeoutMs: 300},
	}})
	if resp, _ := chat(t, ts, body, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("fail-closed rule with a dead receiver must block: %d", resp.StatusCode)
	}
	// fail-open: the same dead receiver lets the prompt through (to the stub engine)
	srv.applyPolicySnapshot(&PolicySnapshot{Revision: "g3", Guardrails: []GuardrailRule{
		{ID: "dead", Phase: "pre", URL: "http://127.0.0.1:1/never", TimeoutMs: 300, FailOpen: true},
	}})
	if resp, _ := chat(t, ts, body, ""); resp.StatusCode == http.StatusForbidden {
		t.Fatal("fail-open rule must not block")
	}
	// rule removed: through
	srv.applyPolicySnapshot(&PolicySnapshot{Revision: "g4"})
	if resp, _ := chat(t, ts, body, ""); resp.StatusCode == http.StatusForbidden {
		t.Fatal("no rules: nothing may block")
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
	// a pre guardrail still runs before the forward: blocked prompts never leave the endpoint
	guard := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "secret") {
			_ = json.NewEncoder(w).Encode(map[string]any{"action": "block", "reason": "secret in prompt"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"action": "rewrite", "replacement": json.RawMessage(setModel(b, "rewritten"))})
	}))
	defer guard.Close()
	srv.applyPolicySnapshot(&PolicySnapshot{Revision: "f2b", Routing: PolicyRouting{FallbackURL: fb.URL},
		Guardrails: []GuardrailRule{{ID: "g", Phase: "pre", URL: guard.URL}}})
	gotModel = ""
	if resp, out := chat(t, ts, `{"model":"m1","messages":[{"role":"user","content":"my secret"}]}`, ""); resp.StatusCode != http.StatusForbidden || !strings.Contains(out, "secret in prompt") {
		t.Fatalf("blocked prompt must not be forwarded: %d %s", resp.StatusCode, out)
	}
	if gotModel != "" {
		t.Fatal("fallback must not see a blocked prompt")
	}
	if resp, _ := chat(t, ts, body, ""); resp.StatusCode != http.StatusOK || gotModel != "rewritten" {
		t.Fatalf("rewritten body must be what the fallback receives: %d model=%q", resp.StatusCode, gotModel)
	}
	// dead fallback → honest 503
	srv.applyPolicySnapshot(&PolicySnapshot{Revision: "f3", Routing: PolicyRouting{FallbackURL: "http://127.0.0.1:1"}})
	if resp, _ := chat(t, ts, body, ""); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("dead fallback: want 503, got %d", resp.StatusCode)
	}
}
