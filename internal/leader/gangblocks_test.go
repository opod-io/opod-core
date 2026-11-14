package leader

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opod-io/opod-core/internal/config"
	"github.com/opod-io/opod-core/internal/engines"
	"github.com/opod-io/opod-core/internal/store"
	"github.com/opod-io/opod-core/pkg/nodeapi"
)

// PLAN T26.22: a gang's head reports the gang engine's blocks, and that
// engine is not the head worker's own — the leader dials it at the
// coordinator. So a gang's prompt is tokenized at the coordinator; the head
// worker's /v1/tokenize would ask an engine that is not there.
func TestAGangsPromptIsTokenizedAtItsCoordinator(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.Listen = ":0"
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := NewServer(cfg, st, &deadEngine{&stubLeaderEngine{}}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)

	tokens := make([]int, 40)
	for i := range tokens {
		tokens[i] = 7 + i
	}
	var coordCalls atomic.Int32
	coord := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tokenize" {
			http.NotFound(w, r)
			return
		}
		var in struct {
			Model    string `json:"model"`
			Messages []struct{ Role, Content string }
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.Model != "m" || len(in.Messages) != 2 || in.Messages[0].Role != "system" {
			t.Errorf("the coordinator was asked %+v", in)
		}
		coordCalls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"tokens": tokens})
	}))
	defer coord.Close()
	head := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the head worker was asked %s: its own engine is not the gang's", r.URL.Path)
		http.Error(w, "no engine here", http.StatusBadGateway)
	}))
	defer head.Close()
	if err := st.Nodes().Upsert(ctx, store.Node{ID: "h1", Hostname: "h1", State: "ready",
		Address: strings.TrimPrefix(head.URL, "http://"), WorkerToken: "t", LastHeartbeat: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := st.Shards().Create(ctx, store.Shard{ID: "c", ModelID: "m", GangID: "g0", Role: "coordinator", NodeID: "h1",
		Address: strings.TrimPrefix(coord.URL, "http://"), ProcessID: "p", Status: "ready", ConfigJSON: `{"engine":"vllm"}`,
		CreatedAt: time.Now(), LastSeen: time.Now()}); err != nil {
		t.Fatal(err)
	}
	want := nodeapi.BlockHashes(tokens, 16)
	// The head reports the gang engine's blocks on its heartbeat, as any worker
	// does — its own engine runs nothing, so it reports no load and no models.
	if err := srv.HeartbeatNode(ctx, HeartbeatRequest{ID: "h1", KVBlocks: &nodeapi.KVBlocks{Seq: 1, BlockSize: 16, Stored: want[:2]}}, Caller{Admin: true}); err != nil {
		t.Fatal(err)
	}

	req := engines.ChatRequest{Model: "m", System: "be brief", Messages: []engines.Message{{Role: "user", Content: "hi"}}}
	got := srv.resolveBlocks(ctx, req)
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("chain %v, want %v", got, want)
	}
	if coordCalls.Load() != 1 {
		t.Fatalf("the coordinator tokenized %d times, want once", coordCalls.Load())
	}
	if srv.prefix.leading("h1", got) != 2 {
		t.Fatal("the head holds the first two blocks of this prompt")
	}
}
