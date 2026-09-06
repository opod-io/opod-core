package controlplane

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/agent"
	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/models"
	"github.com/opod-io/opod/internal/store"
)

// TestTwoNode_RegisterAndHeartbeat is the in-process equivalent of running
// `opod up` on a leader and `opod join` on a worker. It exercises the
// register → heartbeat → placement-reconciliation wire protocol without
// needing two physical machines.
//
// This test exists because the README has historically carried a "multi-node
// routing landed but not yet tested with two physical machines" caveat. Real
// hardware verification still belongs in docs/TWO_NODE_VERIFICATION.md, but
// this test catches regressions in the protocol the agent and leader speak
// to each other.
func TestTwoNode_RegisterAndHeartbeat(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// --- Leader side ---
	cfg := config.Default()
	cfg.Listen = ":0"
	cfg.Auth.RequireKeys = true

	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	// Mint a node-scope API key so the agent can authenticate.
	plain, rec, err := auth.Generate("worker-test", "node", "")
	if err != nil {
		t.Fatalf("auth.Generate: %v", err)
	}
	if err := st.APIKeys().Create(ctx, rec); err != nil {
		t.Fatalf("APIKeys().Create: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	leader := NewServer(cfg, st, &stubLeaderEngine{}, nil, log, nil)

	leaderHTTP := httptest.NewServer(leader.routes())
	defer leaderHTTP.Close()

	// --- Worker side ---
	a := &agent.Agent{
		NodeID:    "test-worker-1",
		LeaderURL: leaderHTTP.URL,
		Token:     plain,
		Address:   "127.0.0.1:9000",
		Capabilities: agent.Capabilities{
			Hostname: "test-worker-1.local",
			OS:       "darwin",
			Arch:     "arm64",
			RAMGB:    16,
		},
		Engine:            &stubWorkerEngine{loaded: []string{"llama-3.2-3b", "qwen-coder-7b"}},
		HeartbeatInterval: 50 * time.Millisecond,
		Log:               log,
		HTTP:              &http.Client{Timeout: 5 * time.Second},
	}

	// --- 1) Register ---
	if err := a.Register(ctx); err != nil {
		t.Fatalf("agent Register: %v", err)
	}

	// Node row exists with the right address + ready state.
	n, err := st.Nodes().Get(ctx, a.NodeID)
	if err != nil {
		t.Fatalf("Nodes().Get: %v", err)
	}
	if n == nil {
		t.Fatal("node not found in store after Register")
	}
	if n.Address != "127.0.0.1:9000" {
		t.Errorf("node address = %q, want 127.0.0.1:9000", n.Address)
	}
	if n.State != "ready" {
		t.Errorf("node state = %q, want ready", n.State)
	}
	if n.WorkerToken != plain {
		t.Errorf("worker token not persisted (got %q, want %q)", n.WorkerToken, plain)
	}

	// --- 2) Heartbeat carrying loaded models ---
	code, err := a.Heartbeat(ctx)
	if err != nil {
		t.Fatalf("agent Heartbeat: %v (code=%d)", err, code)
	}
	if code >= 400 {
		t.Fatalf("heartbeat returned %d", code)
	}

	// Placements row appears for each loaded model, attributed to this node.
	pls, err := st.Placements().GetByNode(ctx, a.NodeID)
	if err != nil {
		t.Fatalf("Placements().GetByNode: %v", err)
	}
	got := map[string]bool{}
	for _, p := range pls {
		got[p.ModelID] = true
	}
	for _, want := range []string{"llama-3.2-3b", "qwen-coder-7b"} {
		if !got[want] {
			t.Errorf("placement for %q not reconciled (got %v)", want, got)
		}
	}

	// --- 3) Heartbeat with an updated loaded list reconciles. ---
	a.Engine = &stubWorkerEngine{loaded: []string{"llama-3.2-3b"}}
	if _, err := a.Heartbeat(ctx); err != nil {
		t.Fatalf("second heartbeat: %v", err)
	}
	pls, _ = st.Placements().GetByNode(ctx, a.NodeID)
	if len(pls) != 1 || pls[0].ModelID != "llama-3.2-3b" {
		t.Errorf("after second heartbeat, expected only llama-3.2-3b in placements, got %d entries: %+v", len(pls), pls)
	}
}

// TestTwoNode_UnauthorizedHeartbeat asserts the agent's 401 short-circuit
// (a revoked token must not loop forever burning CPU).
func TestTwoNode_UnauthorizedHeartbeat(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cfg := config.Default()
	cfg.Listen = ":0"
	cfg.Auth.RequireKeys = true

	st, _ := store.OpenSQLite(":memory:")
	defer st.Close()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	leader := NewServer(cfg, st, &stubLeaderEngine{}, nil, log, nil)
	leaderHTTP := httptest.NewServer(leader.routes())
	defer leaderHTTP.Close()

	a := &agent.Agent{
		NodeID:    "rogue",
		LeaderURL: leaderHTTP.URL,
		Token:     "sk-orc-not-a-real-key",
		Address:   "127.0.0.1:9001",
		Engine:    &stubWorkerEngine{},
		Log:       log,
		HTTP:      &http.Client{Timeout: 5 * time.Second},
	}

	code, err := a.Heartbeat(ctx)
	if err == nil {
		t.Fatal("expected unauthorized error, got nil")
	}
	if code != 401 && code != 403 {
		t.Errorf("expected 401/403, got %d (err=%v)", code, err)
	}
	// Sanity: error message surfaces the unauthorized status (used by Loop's
	// switch on http.StatusUnauthorized to break out cleanly).
	if !strings.Contains(err.Error(), "401") && !strings.Contains(err.Error(), "403") &&
		!strings.Contains(err.Error(), "Unauthorized") && !strings.Contains(err.Error(), "Forbidden") {
		t.Errorf("error message doesn't surface unauthorized: %v", err)
	}
}

// stubLeaderEngine is the leader's own local engine; for these tests we only
// need it to satisfy the engines.Engine interface — nothing exercises it.
type stubLeaderEngine struct{}

func (s *stubLeaderEngine) Name() string                               { return "stub-leader" }
func (s *stubLeaderEngine) Endpoint() string                           { return "stub://leader" }
func (s *stubLeaderEngine) Health(ctx context.Context) error           { return nil }
func (s *stubLeaderEngine) List(ctx context.Context) ([]string, error) { return nil, nil }
func (s *stubLeaderEngine) Pull(ctx context.Context, _ string, _ func(string, int64, int64)) error {
	return nil
}
func (s *stubLeaderEngine) Delete(ctx context.Context, _ string) error { return nil }
func (s *stubLeaderEngine) Unload(ctx context.Context, _ string) error { return nil }
func (s *stubLeaderEngine) Chat(ctx context.Context, req engines.ChatRequest) (<-chan engines.StreamEvent, error) {
	return nil, nil
}
func (s *stubLeaderEngine) Embed(ctx context.Context, req engines.EmbedRequest) (engines.EmbedResponse, error) {
	return engines.EmbedResponse{}, nil
}

// stubWorkerEngine reports a fixed loaded-models list to the agent so we can
// verify the leader reconciles placements correctly.
type stubWorkerEngine struct {
	loaded []string
}

func (s *stubWorkerEngine) Name() string                               { return "stub-worker" }
func (s *stubWorkerEngine) Endpoint() string                           { return "stub://worker" }
func (s *stubWorkerEngine) Health(ctx context.Context) error           { return nil }
func (s *stubWorkerEngine) List(ctx context.Context) ([]string, error) { return s.loaded, nil }
func (s *stubWorkerEngine) Pull(ctx context.Context, _ string, _ func(string, int64, int64)) error {
	return nil
}
func (s *stubWorkerEngine) Delete(ctx context.Context, _ string) error { return nil }
func (s *stubWorkerEngine) Unload(ctx context.Context, _ string) error { return nil }
func (s *stubWorkerEngine) Chat(ctx context.Context, req engines.ChatRequest) (<-chan engines.StreamEvent, error) {
	return nil, nil
}
func (s *stubWorkerEngine) Embed(ctx context.Context, req engines.EmbedRequest) (engines.EmbedResponse, error) {
	return engines.EmbedResponse{}, nil
}

// silence unused-import lint when models is touched elsewhere
var _ = models.Entry{}
