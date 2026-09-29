package agent

// Prefix-cache block events and the tokenize route (feature
// "kv_block_events"): the worker's half of routing by what an engine holds.

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/opod-io/opod-sdk/nodeapi"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/kvevents"
)

var kvEventsOnce sync.Once

// kvEventsArgs is the engine flag that turns the publisher on, bound to
// localhost like every engine port. Empty when the feature is off.
func (s *Server) kvEventsArgs() string {
	if !s.KVEvents || s.Blocks == nil {
		return ""
	}
	return "--kv-events-config '" + kvevents.EngineConfig("") + "'"
}

// startKVEvents starts the subscriber once per worker process. It outlives
// every engine restart: the subscriber reconnects, and a reconnect is a reset.
func (s *Server) startKVEvents() {
	if !s.KVEvents || s.Blocks == nil {
		return
	}
	kvEventsOnce.Do(func() {
		go kvevents.Subscribe(context.Background(), "", s.Blocks, s.logf)
	})
}

// tokenize answers nodeapi.PathTokenize from the engine's own tokenizer. 501
// when the engine has none for a caller; the leader then keeps its sticky pin.
func (s *Server) tokenize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	tk, ok := s.Engine.(engines.Tokenizer)
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "unsupported", "engine": s.Engine.Name()})
		return
	}
	var in nodeapi.TokenizeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&in); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	msgs := make([]engines.Message, 0, len(in.Messages))
	for _, m := range in.Messages {
		msgs = append(msgs, engines.Message{Role: m.Role, Content: m.Content})
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	// The engine knows the model by the name it was launched to serve.
	tokens, err := tk.Tokenize(ctx, s.Aliases.Native(in.Model), msgs, in.Prompt, in.MaxTokens)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(nodeapi.TokenizeResponse{Tokens: tokens})
}
