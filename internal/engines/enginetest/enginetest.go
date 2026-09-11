// Package enginetest is the engine conformance suite: one table of checks
// every driver must pass (registry identity, Health, List, Pull, Chat
// streaming, Embed, Unload semantics, error classification, native-name
// resolution). A driver package supplies a Fixture — its fake upstream and
// what the fake advertises — and calls Run from its own test:
//
//	func TestConformance(t *testing.T) {
//		enginetest.Run(t, enginetest.Fixture{Name: "vllm", Backend: ..., Expect: ...})
//	}
//
// Adding a driver without a conformance test is the one thing this package
// exists to make awkward.
package enginetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/engines"
)

// Fixture describes one driver under test.
type Fixture struct {
	// Name is the registry name the suite constructs through (canonical or alias).
	Name string
	// Canonical is the expected Engine.Name(); defaults to Name.
	Canonical string
	// Backend is the fake upstream speaking the engine's wire protocol. It
	// must serve everything Expect promises.
	Backend http.Handler
	// Expect states what Backend advertises and which optional interfaces
	// the driver implements.
	Expect Expect
	// NativeNames are (source → expected native name) cases for this engine.
	NativeNames []NativeNameCase
}

// Expect is the observable behaviour the suite asserts against Backend.
type Expect struct {
	Models   []string       // List() result
	Chat     string         // concatenated deltas Backend streams for any chat
	Usage    *engines.Usage // Usage on the Done event; nil = not asserted
	Reason   string         // finish reason on the Done event; "" = not asserted
	Embeds   bool           // driver implements engines.EmbedEngine; Backend answers one vector per input
	Unloads  bool           // Unload against Backend returns nil; false ⇒ must return ErrUnloadNotSupported
	Resident bool           // driver implements engines.ResidentLister and Backend serves it
	Loads    bool           // driver implements engines.Loader and Backend serves it
}

// NativeNameCase is one NativeName expectation.
type NativeNameCase struct {
	Source engines.Source
	Want   string
}

const streamTimeout = 5 * time.Second

// Run executes the conformance table for f.
func Run(t *testing.T, f Fixture) {
	t.Helper()
	canonical := f.Canonical
	if canonical == "" {
		canonical = f.Name
	}
	desc, ok := engines.Lookup(f.Name)
	if !ok {
		t.Fatalf("engine %q is not registered (linked: %v)", f.Name, engines.Names())
	}
	if desc.Name != canonical {
		t.Fatalf("Lookup(%q).Name = %q, want canonical %q", f.Name, desc.Name, canonical)
	}
	if desc.StartHint == "" {
		t.Errorf("descriptor %q has no StartHint", desc.Name)
	}

	srv := httptest.NewServer(f.Backend)
	t.Cleanup(srv.Close)
	// Trailing slash on purpose: drivers must normalise the endpoint.
	eng, err := engines.NewWithAuth(f.Name, srv.URL+"/", "test-key")
	if err != nil {
		t.Fatalf("NewWithAuth: %v", err)
	}
	if eng == nil {
		t.Fatal("NewWithAuth returned nil engine")
	}
	ctx := context.Background()
	model := "m"
	if len(f.Expect.Models) > 0 {
		model = f.Expect.Models[0]
	}

	t.Run("identity", func(t *testing.T) {
		if got := eng.Name(); got != canonical {
			t.Errorf("Name() = %q, want %q", got, canonical)
		}
		if got := eng.Endpoint(); got != srv.URL {
			t.Errorf("Endpoint() = %q, want %q (trailing slash trimmed)", got, srv.URL)
		}
	})

	t.Run("health", func(t *testing.T) {
		if err := eng.Health(ctx); err != nil {
			t.Errorf("Health: %v", err)
		}
	})

	t.Run("list", func(t *testing.T) {
		got, err := eng.List(ctx)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if !reflect.DeepEqual(got, f.Expect.Models) {
			t.Errorf("List = %v, want %v", got, f.Expect.Models)
		}
	})

	t.Run("pull", func(t *testing.T) {
		if err := eng.Pull(ctx, model, nil); err != nil {
			t.Errorf("Pull(nil progress): %v", err)
		}
		if err := eng.Pull(ctx, model, func(string, int64, int64) {}); err != nil {
			t.Errorf("Pull(progress): %v", err)
		}
	})

	t.Run("chat", func(t *testing.T) {
		ch, err := eng.Chat(ctx, engines.ChatRequest{
			Model:    model,
			Messages: []engines.Message{{Role: "user", Content: "hi"}},
			Stream:   true,
		})
		if err != nil {
			t.Fatalf("Chat: %v", err)
		}
		evs := drain(t, ch)
		var text strings.Builder
		var dones []engines.StreamEvent
		for i, ev := range evs {
			if ev.Err != nil {
				t.Fatalf("event %d carries error: %v", i, ev.Err)
			}
			text.WriteString(ev.Delta)
			if ev.Done {
				dones = append(dones, ev)
				if i != len(evs)-1 {
					t.Errorf("Done event at index %d is not last of %d", i, len(evs))
				}
			}
		}
		if len(dones) != 1 {
			t.Fatalf("got %d Done events, want exactly 1 (%d events total)", len(dones), len(evs))
		}
		if got := text.String(); got != f.Expect.Chat {
			t.Errorf("streamed text = %q, want %q", got, f.Expect.Chat)
		}
		done := dones[0]
		if f.Expect.Reason != "" && done.Reason != f.Expect.Reason {
			t.Errorf("finish reason = %q, want %q", done.Reason, f.Expect.Reason)
		}
		if f.Expect.Usage != nil {
			if done.Usage == nil {
				t.Errorf("Done event has no Usage, want %+v", *f.Expect.Usage)
			} else if *done.Usage != *f.Expect.Usage {
				t.Errorf("Usage = %+v, want %+v", *done.Usage, *f.Expect.Usage)
			}
		}
	})

	t.Run("chat/cancelled-context", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		ch, err := eng.Chat(cctx, engines.ChatRequest{Model: model, Messages: []engines.Message{{Role: "user", Content: "hi"}}, Stream: true})
		if err != nil {
			return // refused synchronously — fine
		}
		drain(t, ch) // must close; drain fails the test on timeout
	})

	t.Run("embed", func(t *testing.T) {
		ee, implements := eng.(engines.EmbedEngine)
		if implements != f.Expect.Embeds {
			t.Fatalf("implements EmbedEngine = %v, fixture says %v", implements, f.Expect.Embeds)
		}
		if !implements {
			return
		}
		resp, err := ee.Embed(ctx, engines.EmbedRequest{Model: model, Inputs: []string{"a", "b"}})
		if err != nil {
			t.Fatalf("Embed: %v", err)
		}
		if len(resp.Vectors) != 2 {
			t.Fatalf("Embed returned %d vectors for 2 inputs", len(resp.Vectors))
		}
		// The backend answers out of order; the driver must restore the caller's.
		if len(resp.Vectors[0]) == 0 || resp.Vectors[0][0] != 0 || len(resp.Vectors[1]) == 0 || resp.Vectors[1][0] != 1 {
			t.Errorf("vectors are not in the caller's order: %v", resp.Vectors)
		}
		if _, err := ee.Embed(ctx, engines.EmbedRequest{Model: model}); err == nil {
			t.Error("Embed with no inputs must error")
		}
	})

	t.Run("unload", func(t *testing.T) {
		err := eng.Unload(ctx, model)
		switch {
		case f.Expect.Unloads && err != nil:
			t.Errorf("Unload: %v", err)
		case !f.Expect.Unloads && !errors.Is(err, engines.ErrUnloadNotSupported):
			t.Errorf("Unload = %v, want ErrUnloadNotSupported", err)
		}
	})

	t.Run("resident", func(t *testing.T) {
		rl, implements := eng.(engines.ResidentLister)
		if implements != f.Expect.Resident {
			t.Fatalf("implements ResidentLister = %v, fixture says %v", implements, f.Expect.Resident)
		}
		if implements {
			if _, err := rl.Resident(ctx); err != nil {
				t.Errorf("Resident: %v", err)
			}
		}
	})

	t.Run("load", func(t *testing.T) {
		ld, implements := eng.(engines.Loader)
		if implements != f.Expect.Loads {
			t.Fatalf("implements Loader = %v, fixture says %v", implements, f.Expect.Loads)
		}
		if implements {
			if err := ld.Load(ctx, model, true); err != nil {
				t.Errorf("Load: %v", err)
			}
		}
	})

	t.Run("errors/unreachable", func(t *testing.T) {
		dead := httptest.NewServer(http.NotFoundHandler())
		url := dead.URL
		dead.Close()
		e2, err := engines.NewWithAuth(f.Name, url, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := e2.Health(ctx); !errors.Is(err, engines.ErrUnreachable) {
			t.Errorf("Health against dead server = %v, want ErrUnreachable", err)
		}
		if _, err := e2.List(ctx); !errors.Is(err, engines.ErrUnreachable) {
			t.Errorf("List against dead server = %v, want ErrUnreachable", err)
		}
		ch, err := e2.Chat(ctx, engines.ChatRequest{Model: model, Messages: []engines.Message{{Role: "user", Content: "hi"}}, Stream: true})
		if !errors.Is(err, engines.ErrUnreachable) {
			t.Errorf("Chat against dead server = %v, want ErrUnreachable", err)
		}
		if ch != nil {
			drain(t, ch)
		}
	})

	t.Run("errors/upstream", func(t *testing.T) {
		bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
		}))
		t.Cleanup(bad.Close)
		e3, err := engines.NewWithAuth(f.Name, bad.URL, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := e3.Health(ctx); !errors.Is(err, engines.ErrUpstream) {
			t.Errorf("Health against 500 = %v, want ErrUpstream", err)
		}
		if _, err := e3.List(ctx); !errors.Is(err, engines.ErrUpstream) {
			t.Errorf("List against 500 = %v, want ErrUpstream", err)
		}
		ch, err := e3.Chat(ctx, engines.ChatRequest{Model: model, Messages: []engines.Message{{Role: "user", Content: "hi"}}, Stream: true})
		if !errors.Is(err, engines.ErrUpstream) {
			t.Errorf("Chat against 500 = %v, want ErrUpstream", err)
		}
		if err != nil && !strings.Contains(err.Error(), "boom") {
			t.Errorf("upstream error must carry the response body, got %q", err.Error())
		}
		if ch != nil {
			drain(t, ch)
		}
	})

	t.Run("nativename", func(t *testing.T) {
		if len(f.NativeNames) == 0 {
			t.Skip("fixture declares no NativeName cases")
		}
		for _, c := range f.NativeNames {
			if got := engines.NativeName(f.Name, c.Source); got != c.Want {
				t.Errorf("NativeName(%q, %+v) = %q, want %q", f.Name, c.Source, got, c.Want)
			}
			// Round trip: what the engine reports must map back to the catalog id.
			if got := engines.CatalogID(f.Name, c.Want, []engines.Source{c.Source}); got != c.Source.ID {
				t.Errorf("CatalogID(%q, %q) = %q, want %q", f.Name, c.Want, got, c.Source.ID)
			}
		}
	})
}

// drain collects every event until the channel closes, failing the test if
// the driver leaves it open — consumers range over it, so an unclosed
// channel is a goroutine leak in production.
func drain(t *testing.T, ch <-chan engines.StreamEvent) []engines.StreamEvent {
	t.Helper()
	var evs []engines.StreamEvent
	timer := time.NewTimer(streamTimeout)
	defer timer.Stop()
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return evs
			}
			evs = append(evs, ev)
		case <-timer.C:
			t.Fatalf("stream not closed after %s (%d events so far)", streamTimeout, len(evs))
			return evs
		}
	}
}

// OpenAIBackend is a fake OpenAI-compatible server (vLLM / MLX-LM /
// llama-server shape): GET /v1/models lists models, POST /v1/chat/completions
// streams deltas as SSE chunks followed by a finish_reason chunk, a
// usage-only chunk (the vLLM/MLX quirk the decoder must handle) and [DONE].
// GET /health answers 200 for llama-server's probe.
func OpenAIBackend(models, deltas []string, usage engines.Usage) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		data := make([]map[string]any, 0, len(models))
		for _, m := range models {
			data = append(data, map[string]any{"id": m, "object": "model"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		write := func(v any) {
			b, _ := json.Marshal(v)
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
		for _, d := range deltas {
			write(map[string]any{"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": d}, "finish_reason": nil}}})
		}
		write(map[string]any{"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}})
		write(map[string]any{"choices": []any{}, "usage": map[string]int{
			"prompt_tokens": usage.PromptTokens, "completion_tokens": usage.CompletionTokens, "total_tokens": usage.TotalTokens,
		}})
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	mux.HandleFunc("POST /v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Input) == 0 {
			http.Error(w, "input required", http.StatusBadRequest)
			return
		}
		// Answer out of order on purpose: a driver must place vectors by index,
		// not by the position the server happened to return them in.
		data := make([]map[string]any, 0, len(body.Input))
		for i := len(body.Input) - 1; i >= 0; i-- {
			data = append(data, map[string]any{"index": i, "embedding": []float32{float32(i), 0.5}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list", "data": data,
			"usage": map[string]int{"prompt_tokens": usage.PromptTokens, "total_tokens": usage.PromptTokens},
		})
	})
	return mux
}
