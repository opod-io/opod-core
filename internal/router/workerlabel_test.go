package router

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/engines"
	_ "github.com/opod-io/opod/internal/engines/all"
	"github.com/opod-io/opod/internal/store"
)

// The leader reaches every worker through the OpenAI-wire driver, whatever the
// worker runs. A llama.cpp worker's failure used to reach the caller as
// "vllm chat: 502 …": the error named the driver the leader spoke with, not the
// engine that failed. It names the engine the worker registered now, and a
// neutral "worker" when the worker did not register one.
func TestAWorkersErrorNamesTheEngineItRegistered(t *testing.T) {
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "engine: llamacpp unreachable", http.StatusBadGateway)
	}))
	defer worker.Close()
	addr := strings.TrimPrefix(worker.URL, "http://")

	r := New(nil, nil)
	node := &store.Node{ID: "n1", Address: addr, HardwareJSON: `{"Engine":"llamacpp"}`}
	if got := r.capsOf(node).engine; got != "llamacpp" {
		t.Fatalf("capsOf engine = %q, want llamacpp", got)
	}
	for _, tc := range []struct{ id, engine, want string }{
		{"n1", "llamacpp", "llamacpp chat: 502"},
		{"n2", "", "worker chat: 502"},
	} {
		eng := r.getOrCreateRemote(tc.id, addr, "tok", tc.engine)
		_, err := eng.Chat(context.Background(), engines.ChatRequest{Model: "m", Messages: []engines.Message{{Role: "user", Content: "hi"}}})
		if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
			t.Fatalf("engine %q: error = %v, want it to start %q", tc.engine, err, tc.want)
		}
		if strings.Contains(err.Error(), "vllm") {
			t.Fatalf("engine %q: the error names vllm: %v", tc.engine, err)
		}
	}
}
