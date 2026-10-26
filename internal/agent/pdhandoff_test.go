package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/fetch"
)

// oneShotRecorder is an engine that can also answer without a stream, as the
// vLLM driver can (openaicompat.Client.ChatOnce).
type oneShotRecorder struct {
	chatRecorder
	once engines.ChatRequest
}

func (o *oneShotRecorder) ChatOnce(_ context.Context, req engines.ChatRequest) (engines.ChatAnswer, error) {
	o.once = req
	return engines.ChatAnswer{Text: "x", FinishReason: "length",
		KVTransfer: json.RawMessage(`{"remote_engine_id":"e1","remote_block_ids":[[1]]}`)}, nil
}

// PLAN T16.9: the prefill step reaches the worker as a non-streamed request
// carrying the leader's kv_transfer_params; the worker asks its engine without
// a stream and hands the engine's kv_transfer_params back. The decode step
// streams as always, with the params passed to the engine as the typed field,
// never as an extra.
func TestTheWorkerRunsBothStepsOfAHandoff(t *testing.T) {
	eng := &oneShotRecorder{}
	s := &Server{Engine: eng}

	w := httptest.NewRecorder()
	s.chatCompletions(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":1,"kv_transfer_params":{"do_remote_decode":true}}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("prefill step: status %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(string(eng.once.KVTransfer), `"do_remote_decode":true`) {
		t.Fatalf("the engine's one-shot call did not carry the leader's params: %s", eng.once.KVTransfer)
	}
	var ans map[string]json.RawMessage
	_ = json.Unmarshal(w.Body.Bytes(), &ans)
	if !strings.Contains(string(ans["kv_transfer_params"]), `"remote_engine_id":"e1"`) {
		t.Fatalf("the worker must hand the engine's params back: %s", w.Body.String())
	}

	w = httptest.NewRecorder()
	s.chatCompletions(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true,"kv_transfer_params":{"remote_engine_id":"e1"}}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("decode step: status %d: %s", w.Code, w.Body.String())
	}
	if string(eng.got.KVTransfer) != `{"remote_engine_id":"e1"}` {
		t.Fatalf("the decode step's params must reach the engine as the typed field: %s", eng.got.KVTransfer)
	}
	if _, extra := eng.got.Extra["kv_transfer_params"]; extra {
		t.Fatal("kv_transfer_params rode as an extra")
	}
}

// An engine that cannot answer without a stream refuses a prefill step by
// name (the leader then serves the request whole), never with a stream.
func TestAPrefillStepOnAnEngineWithNoOneShotIsRefused(t *testing.T) {
	s := &Server{Engine: &chatRecorder{}}
	w := httptest.NewRecorder()
	s.chatCompletions(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"kv_transfer_params":{"do_remote_decode":true}}`)))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "unsupported_request") {
		t.Fatalf("want 400 unsupported_request, got %d: %s", w.Code, w.Body.String())
	}
}

// The penalty gate's source (T16.9 §6): the served model's own
// generation_config.json, read from the directory the engine loads. No file
// there is the engine's default (no penalty); a model the engine pulls by repo
// name has no directory here and is not known.
func TestTheWorkerReadsTheServedModelsSamplingDefaults(t *testing.T) {
	dir := t.TempDir()
	snapshot := func(repo string, files map[string]string) {
		sdir := fetch.SnapshotDir(dir, repo, "0123abc")
		if err := os.MkdirAll(sdir, 0o755); err != nil {
			t.Fatal(err)
		}
		var listed []fetch.SnapshotFile
		for name, body := range files {
			if err := os.WriteFile(filepath.Join(sdir, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			listed = append(listed, fetch.SnapshotFile{Name: name, Size: int64(len(body))})
		}
		man, _ := json.Marshal(fetch.SnapshotManifest{Repo: repo, Revision: "0123abc", Files: listed})
		if err := os.WriteFile(filepath.Join(sdir, fetch.SnapshotManifestName), man, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	snapshot("org/penalised", map[string]string{"model.safetensors": "w", "generation_config.json": `{"repetition_penalty":1.1,"top_k":20}`})
	snapshot("org/plain", map[string]string{"model.safetensors": "w", "generation_config.json": `{"top_k":20}`})
	snapshot("org/nofile", map[string]string{"model.safetensors": "w"})
	s := &Server{ModelsDir: dir, ModelRevision: "0123abc"}
	for model, want := range map[string]SamplingDefaults{
		"org/penalised": {Known: true, RepetitionPenalty: 1.1},
		"org/plain":     {Known: true, RepetitionPenalty: 1},
		"org/nofile":    {Known: true, RepetitionPenalty: 1},
		"org/pulled":    {},
	} {
		if got := s.samplingDefaultsOf(model); got != want {
			t.Errorf("%s: %+v, want %+v", model, got, want)
		}
	}
}

// scriptedEngine streams the events it is given.
type scriptedEngine struct {
	chatRecorder
	events []engines.StreamEvent
}

func (s *scriptedEngine) Chat(_ context.Context, req engines.ChatRequest) (<-chan engines.StreamEvent, error) {
	s.got = req
	ch := make(chan engines.StreamEvent, len(s.events))
	for _, ev := range s.events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

// vLLM fails a KV load it cannot complete by ending the decode step's stream
// with no token and no finish_reason; the worker must say so as an error,
// never write it as a clean "stop" (design-partner cell, 2026-10-26: every
// split answered "" after a prefill worker's restart changed its NIXL hash).
// A normal answer, and a request with no KV reference, are untouched.
func TestADecodeStepThatProducesNothingIsAnError(t *testing.T) {
	silent := []engines.StreamEvent{{Done: true}}
	for _, c := range []struct {
		name, body string
		events     []engines.StreamEvent
		failed     bool
	}{
		{"silent decode step", `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}],"kv_transfer_params":{"remote_engine_id":"e"}}`, silent, true},
		{"decode step that answers", `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}],"kv_transfer_params":{"remote_engine_id":"e"}}`, []engines.StreamEvent{{Delta: "Paris"}, {Done: true, Reason: "stop"}}, false},
		{"no KV reference", `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`, silent, false},
	} {
		s := &Server{Engine: &scriptedEngine{events: c.events}}
		w := httptest.NewRecorder()
		s.chatCompletions(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(c.body)))
		out := w.Body.String()
		if got := strings.Contains(out, "kv load failed"); got != c.failed {
			t.Errorf("%s: kv-load error = %v, want %v:\n%s", c.name, got, c.failed, out)
		}
		if c.failed && strings.Contains(out, `"finish_reason":"stop"`) {
			t.Errorf("%s: a failed load must not end as a clean stop:\n%s", c.name, out)
		}
	}
}
