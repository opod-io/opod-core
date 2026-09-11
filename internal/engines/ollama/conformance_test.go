package ollama_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/engines/enginetest"
)

// fakeOllama speaks enough of the Ollama HTTP API for the conformance suite.
func fakeOllama(models []string, deltas []string, promptTokens, evalTokens int) http.Handler {
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }
	mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{"version": "0.0.0-test"})
	})
	mux.HandleFunc("GET /api/tags", func(w http.ResponseWriter, r *http.Request) {
		list := make([]map[string]any, 0, len(models))
		for _, m := range models {
			list = append(list, map[string]any{"name": m})
		}
		writeJSON(w, map[string]any{"models": list})
	})
	mux.HandleFunc("GET /api/ps", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"models": []map[string]any{{"name": models[0], "size": 100, "size_vram": 100}}})
	})
	mux.HandleFunc("POST /api/pull", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"status":"pulling manifest"}`)
		fmt.Fprintln(w, `{"status":"downloading","digest":"sha256:abc","total":100,"completed":50}`)
		fmt.Fprintln(w, `{"status":"success"}`)
	})
	mux.HandleFunc("POST /api/generate", func(w http.ResponseWriter, r *http.Request) { // load + unload
		writeJSON(w, map[string]any{"done": true})
	})
	mux.HandleFunc("DELETE /api/delete", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{})
	})
	mux.HandleFunc("POST /api/embed", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		// One distinguishable vector per input: the suite checks that a driver
		// returns them in the caller's order, which identical vectors cannot show.
		vecs := make([][]float32, len(req.Input))
		for i := range vecs {
			vecs[i] = []float32{float32(i), 0.5}
		}
		writeJSON(w, map[string]any{"embeddings": vecs, "prompt_eval_count": 3})
	})
	mux.HandleFunc("POST /api/chat", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		for _, d := range deltas {
			writeJSON(w, map[string]any{"message": map[string]string{"role": "assistant", "content": d}, "done": false})
		}
		writeJSON(w, map[string]any{
			"message": map[string]string{"role": "assistant", "content": ""}, "done": true, "done_reason": "stop",
			"prompt_eval_count": promptTokens, "eval_count": evalTokens,
		})
	})
	return mux
}

func TestConformance(t *testing.T) {
	enginetest.Run(t, enginetest.Fixture{
		Name:    "ollama",
		Backend: fakeOllama([]string{"llama3.1:8b", "nomic-embed-text:latest"}, []string{"Hi ", "there"}, 9, 2),
		Expect: enginetest.Expect{
			Models:   []string{"llama3.1:8b", "nomic-embed-text:latest"},
			Chat:     "Hi there",
			Usage:    &engines.Usage{PromptTokens: 9, CompletionTokens: 2, TotalTokens: 11},
			Reason:   "stop",
			Embeds:   true,
			Unloads:  true,
			Resident: true,
			Loads:    true,
		},
		NativeNames: []enginetest.NativeNameCase{
			{Source: engines.Source{ID: "llama-3-1-8b", OllamaName: "llama3.1:8b", Repo: "meta-llama/Llama-3.1-8B-Instruct"}, Want: "llama3.1:8b"},
			{Source: engines.Source{ID: "hf-only", Repo: "org/repo"}, Want: "hf-only"},
		},
	})
}
