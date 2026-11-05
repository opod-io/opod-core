package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/opod-io/opod-sdk/nodeapi"
	"github.com/opod-io/opod/internal/kvevents"
)

// A heartbeat that did not land — here a 404 from a leader that restarted and
// does not know this worker — is followed by one that states every block
// held, Cleared first, so the new leader's index is whole again (T24.56: a hot
// shared block the engine never stores again otherwise stays missing, and no
// request's chain matches anything).
func TestAFailedHeartbeatIsFollowedByAFullBlockStatement(t *testing.T) {
	tr := kvevents.NewTranslator()
	tok := make([]any, 32)
	for i := range tok {
		tok[i] = i + 1
	}
	b, _ := msgpack.Marshal([]any{1790000020.0, []any{map[string]any{"type": "BlockStored",
		"block_hashes": []any{uint64(41), uint64(42)}, "parent_block_hash": nil, "token_ids": tok, "block_size": int8(16)}}, int8(0)})
	if err := tr.Frame(0, b); err != nil {
		t.Fatal(err)
	}
	var bodies []*nodeapi.KVBlocks
	calls := 0
	leader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var hb struct {
			KVBlocks *nodeapi.KVBlocks `json:"kv_blocks"`
		}
		_ = json.NewDecoder(r.Body).Decode(&hb)
		bodies = append(bodies, hb.KVBlocks)
		if calls++; calls == 1 {
			http.Error(w, `{"error":{"message":"unknown node — register first"}}`, http.StatusNotFound)
		}
	}))
	defer leader.Close()
	a := &Agent{NodeID: "n1", LeaderURL: leader.URL, Token: "sk", Aliases: &Aliases{}, Blocks: tr,
		HTTP: leader.Client(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if _, err := a.Heartbeat(context.Background()); err == nil {
		t.Fatal("the first heartbeat must fail (404)")
	}
	if _, err := a.Heartbeat(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 || bodies[0] == nil || bodies[1] == nil {
		t.Fatalf("both heartbeats carry blocks: %+v", bodies)
	}
	if !bodies[1].Cleared || len(bodies[1].Stored) != 2 {
		t.Fatalf("after the 404 the heartbeat restates everything held: %+v", bodies[1])
	}
}
