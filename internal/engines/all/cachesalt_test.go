package all_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/opod-io/opod/internal/engines"
	_ "github.com/opod-io/opod/internal/engines/all"
)

// Each driver sends the leader's cache salt to its engine only when the
// engine keys its prefix cache by one (T26.8): vLLM, SGLang and TensorRT-LLM
// (read on the pinned images; TensorRT-LLM also refuses a key it does not
// know), never llama.cpp or MLX. The registry says the same as the drivers.
func TestDriversSendCacheSaltOnlyWhereTheEngineTakesOne(t *testing.T) {
	for _, tc := range []struct {
		engine string
		takes  bool
	}{
		{"vllm", true}, {"sglang", true}, {"trtllm", true},
		{"llamacpp", false}, {"mlx", false},
	} {
		t.Run(tc.engine, func(t *testing.T) {
			if got := engines.TakesCacheSalt(tc.engine); got != tc.takes {
				t.Fatalf("registry: TakesCacheSalt = %v, want %v", got, tc.takes)
			}
			var body map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&body)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			defer srv.Close()
			eng := engines.MustNew(tc.engine, srv.URL, "")
			stream, err := eng.Chat(context.Background(), engines.ChatRequest{Model: "m", Stream: true,
				Messages: []engines.Message{{Role: "user", Content: "hi"}}, CacheSalt: "0123abcd"})
			if err != nil {
				t.Fatal(err)
			}
			for range stream {
			}
			got, present := body["cache_salt"]
			if present != tc.takes || (present && got != "0123abcd") {
				t.Fatalf("cache_salt = %v (present %v), want present %v", got, present, tc.takes)
			}
		})
	}
	if engines.TakesCacheSalt("") || engines.TakesCacheSalt("no-such-engine") {
		t.Fatal("an engine nobody read takes no salt")
	}
}
