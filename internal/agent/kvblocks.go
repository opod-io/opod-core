package agent

// Prefix-cache block events and the tokenize route (feature
// "kv_block_events"): the worker's half of routing by what an engine holds.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/opod-io/opod-core/internal/engines"
	"github.com/opod-io/opod-core/internal/kvevents"
	"github.com/opod-io/opod-core/pkg/nodeapi"
)

var kvEventsOnce sync.Once

// kvEventsArgs is the engine flag that turns the publisher on, bound to
// localhost like every engine port. Empty when the feature is off.
func (s *Server) kvEventsArgs() string {
	if !s.KVEvents || s.Blocks == nil {
		return ""
	}
	return kvEventsConfigArg()
}

func kvEventsConfigArg() string {
	return "--kv-events-config '" + kvevents.EngineConfig("") + "'"
}

// vllmSelfDescribing is the transfer config that makes vLLM's CPU offloading
// describe each block it stores like the card does — token ids, parent hash,
// block size — so the worker can name it with our hash and the leader can
// count it (feature kv_cpu_tier_hits, PLAN T24.56). vLLM merges it with the
// offloading its own --kv-offloading-size sets up (config/vllm.py, 0.33).
const vllmSelfDescribing = `{"kv_role":"kv_both","kv_connector_extra_config":{"self_describing_kv_events":true}}`

// sglangKVPageSize is the page SGLang is started with when it publishes KV
// events and the plan names none: SGLang's default page is one token, so each
// event would name one block per token and the leader's per-worker index would
// hold a few hundred prompts. 16 is vLLM's block, the size the index was
// designed around (T19.21).
const sglangKVPageSize = 16

// KVEventsArgs is the flags that make engine ("vllm" or "sglang") publish its
// prefix-cache events to this worker's subscriber on localhost, given the
// plan's own flags; "" for an engine that publishes none. One rule for a
// worker's own launch and a gang's (the leader's scheduler builds a gang
// head's command with it, PLAN T26.22):
//
//   - vLLM: the publisher, plus the self-describing transfer config when the
//     plan gives the engine a CPU tier (--kv-offloading-size) and states no
//     transfer config of its own;
//   - SGLang: the same publisher config (SGLang reads it with pydantic, which
//     ignores the vLLM-only key), plus a page size unless the plan sets one.
func KVEventsArgs(engine, flagArgs string) string {
	args := kvEventsConfigArg()
	switch engine {
	case "vllm":
		if strings.Contains(flagArgs, "--kv-offloading-size") && !strings.Contains(flagArgs, "--kv-transfer-config") {
			args += " --kv-transfer-config '" + vllmSelfDescribing + "'"
		}
	case "sglang":
		if !strings.Contains(flagArgs, "--page-size") {
			args += fmt.Sprintf(" --page-size %d", sglangKVPageSize)
		}
	default:
		return ""
	}
	return args
}

// vllmKVEventsArgs is KVEventsArgs for this worker's own vLLM launch: nothing
// when the feature is off.
func (s *Server) vllmKVEventsArgs(flagArgs string) string {
	if s.kvEventsArgs() == "" {
		return ""
	}
	return KVEventsArgs("vllm", flagArgs)
}

// sglangKVEventsArgs is KVEventsArgs for this worker's own SGLang launch.
func (s *Server) sglangKVEventsArgs(flagArgs string) string {
	if s.kvEventsArgs() == "" {
		return ""
	}
	return KVEventsArgs("sglang", flagArgs)
}

// startKVEvents starts the subscriber once per worker process. It outlives
// every engine restart: the subscriber reconnects, and a reconnect is a reset.
// Called where this worker launches its own engine, and where its leader
// starts a gang's engine here that publishes to it (ProcessSpec.KVEvents).
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
