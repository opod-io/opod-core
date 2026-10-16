//go:build perfgate

package perfgate

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// stubModel is the one model the stub engine serves.
const stubModel = "stub-model"

// stubEngine speaks the slice of vLLM's HTTP surface a worker uses: the model
// list (health and placement), /metrics (the load scrape) and streamed chat.
// Its timing is fixed — a prefill pause, then one token per gap — so the engine
// contributes the same time to every request and any difference between the
// arms is what sits in front of it.
type stubEngine struct {
	prefill time.Duration
	tokens  int
	gap     time.Duration
}

func (s stubEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"object":"list","data":[{"id":%q,"object":"model","owned_by":"stub"}]}`, stubModel)
	case r.Method == http.MethodGet && r.URL.Path == "/metrics":
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprint(w, "vllm:num_requests_waiting 0\nvllm:kv_cache_usage_perc 0\n")
	case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
		s.chat(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s stubEngine) chat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Stream bool `json:"stream"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	time.Sleep(s.prefill)
	if !req.Stream {
		time.Sleep(time.Duration(s.tokens) * s.gap)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"stub","object":"chat.completion","created":0,"model":%q,`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":1,"completion_tokens":%d,"total_tokens":%d}}`, stubModel, s.tokens, s.tokens+1)
		return
	}
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	chunk := func(delta, finish string) {
		fin := "null"
		if finish != "" {
			fin = fmt.Sprintf("%q", finish)
		}
		fmt.Fprintf(w, `data: {"id":"stub","object":"chat.completion.chunk","created":0,"model":%q,`+
			`"choices":[{"index":0,"delta":%s,"finish_reason":%s}]}`+"\n\n", stubModel, delta, fin)
		if flusher != nil {
			flusher.Flush()
		}
	}
	for i := 0; i < s.tokens; i++ {
		if i > 0 {
			time.Sleep(s.gap)
		}
		chunk(`{"content":"tok "}`, "")
	}
	chunk(`{}`, "stop")
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}
