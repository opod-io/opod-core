package agent

// POST /v1/model/unload (feature worker_unload): every answer the route has,
// through the HTTP route with the worker's own auth, and the property the
// leader depends on — after a 200 the next heartbeat no longer lists the model.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/engines"
	_ "github.com/opod-io/opod/internal/engines/ollama" // the registry's native-name rule for "ollama"
)

// launchedStub is an engine the worker launches itself (vLLM, SGLang,
// llama.cpp): it has no unload, and it lists the model exactly while the
// worker's supervised process for it exists — the process is the model.
type launchedStub struct {
	engines.Engine
	name    string
	sup     *Supervisor
	process string
	model   string
}

func (e *launchedStub) Name() string     { return e.name }
func (e *launchedStub) Endpoint() string { return "http://127.0.0.1:1" }
func (e *launchedStub) Pull(context.Context, string, func(string, int64, int64)) error {
	return nil
}
func (e *launchedStub) Unload(context.Context, string) error { return engines.ErrUnloadNotSupported }
func (e *launchedStub) List(context.Context) ([]string, error) {
	if _, ok := e.sup.Get(e.process); ok {
		return []string{e.model}, nil
	}
	return nil, nil
}

// daemonStub is an engine that outlives its models and can drop one
// (Ollama): installed is what List answers, resident what Resident answers.
type daemonStub struct {
	engines.Engine
	mu        sync.Mutex
	installed []string
	resident  []string
	unloadErr error
	unloaded  []string
}

func (e *daemonStub) Name() string { return "ollama" }
func (e *daemonStub) List(context.Context) ([]string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.installed...), nil
}
func (e *daemonStub) Resident(context.Context) ([]engines.ResidentModel, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]engines.ResidentModel, 0, len(e.resident))
	for _, n := range e.resident {
		out = append(out, engines.ResidentModel{Name: n})
	}
	return out, nil
}
func (e *daemonStub) Unload(_ context.Context, name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.unloadErr != nil {
		return e.unloadErr
	}
	e.unloaded = append(e.unloaded, name)
	kept := e.resident[:0:0]
	for _, n := range e.resident {
		if n != name {
			kept = append(kept, n)
		}
	}
	e.resident = kept
	return nil
}

// externalStub is an engine somebody else started: no unload, and nothing of
// the worker's to stop.
type externalStub struct {
	engines.Engine
	models []string
}

func (e *externalStub) Name() string                           { return "vllm" }
func (e *externalStub) List(context.Context) ([]string, error) { return e.models, nil }
func (e *externalStub) Unload(context.Context, string) error   { return engines.ErrUnloadNotSupported }

func unloadServer(t *testing.T, srv *Server) func(body string) (int, string) {
	t.Helper()
	if srv.Supervisor == nil {
		srv.Supervisor = NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	}
	t.Cleanup(srv.Supervisor.StopAll)
	srv.Token = "sk-test"
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/model/load", srv.auth(srv.modelLoad))
	mux.HandleFunc("/v1/model/unload", srv.auth(srv.modelUnload))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return func(body string) (int, string) {
		path := "/v1/model/unload"
		if strings.HasPrefix(body, "load:") {
			path, body = "/v1/model/load", strings.TrimPrefix(body, "load:")
		}
		r, _ := http.NewRequest(http.MethodPost, ts.URL+path, bytes.NewReader([]byte(body)))
		r.Header.Set("Authorization", "Bearer sk-test")
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
}

func decodeUnload(t *testing.T, body string) unloadModelResponse {
	t.Helper()
	var out unloadModelResponse
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("not an unload answer: %v: %s", err, body)
	}
	return out
}

// heartbeatModels runs one heartbeat of an agent on the same engine and alias
// table and returns the loaded_models the leader would receive.
func heartbeatModels(t *testing.T, eng engines.Engine, al *Aliases) []string {
	t.Helper()
	var got []string
	leader := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		var hb struct {
			LoadedModels []string `json:"loaded_models"`
		}
		_ = json.NewDecoder(r.Body).Decode(&hb)
		got = hb.LoadedModels
	}))
	defer leader.Close()
	a := &Agent{NodeID: "n1", LeaderURL: leader.URL, Token: "sk", Engine: eng, Aliases: al,
		HTTP: leader.Client(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if _, err := a.Heartbeat(context.Background()); err != nil {
		t.Fatal(err)
	}
	return got
}

// A worker that launched its engine for the model stops that process; the
// model is gone from the very next heartbeat, the alias and the LoRA launch
// record with it; asking again is a noop, as is asking for a model the
// engine does not serve.
func TestWorkerUnloadStopsTheEngineItLaunched(t *testing.T) {
	sup := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	al := &Aliases{}
	eng := &launchedStub{name: "llamacpp", sup: sup, process: "llama-server", model: "qwen-gguf"}
	srv := &Server{Engine: eng, Supervisor: sup, Aliases: al}
	call := unloadServer(t, srv)

	// The load is the real handler: it launches `llama-server` under the
	// supervisor (the binary need not exist for the record to) and notes it.
	if code, body := call(`load:{"id":"qwen-gguf","repo":"example/Qwen-GGUF","path":"/models/qwen.gguf"}`); code != 200 {
		t.Fatalf("load: %d %s", code, body)
	}
	if l := srv.launched.Load(); l == nil || l.process != "llama-server" || l.id != "qwen-gguf" {
		t.Fatalf("the load must record what it launched, got %+v", l)
	}
	srv.loraLaunched.Store(&loraLaunch{slots: true, maxRank: 16})
	if got := heartbeatModels(t, eng, al); len(got) != 1 || got[0] != "qwen-gguf" {
		t.Fatalf("before the unload the heartbeat lists the model, got %v", got)
	}

	if code, body := call(`{"id":"other-model","repo":"example/Other"}`); code != 200 || decodeUnload(t, body).Status != "noop" ||
		!strings.Contains(body, "qwen-gguf") {
		t.Fatalf("a model this engine does not serve is a noop naming what it serves: %d %s", code, body)
	}
	if _, ok := sup.Get("llama-server"); !ok {
		t.Fatal("a noop must not stop the engine")
	}

	code, body := call(`{"id":"qwen-gguf","repo":"example/Qwen-GGUF","path":"/models/qwen.gguf"}`)
	if ans := decodeUnload(t, body); code != 200 || ans.Status != "unloaded" || ans.Model == "" {
		t.Fatalf("unload: %d %s", code, body)
	}
	if _, ok := sup.Get("llama-server"); ok {
		t.Error("the supervised engine process must be stopped")
	}
	if got := heartbeatModels(t, eng, al); len(got) != 0 {
		t.Errorf("after a 200 the next heartbeat must not list the model, got %v", got)
	}
	if srv.launched.Load() != nil || srv.loraLaunched.Load() != nil {
		t.Error("the launch record and the LoRA launch record must be cleared")
	}

	if code, body := call(`{"id":"qwen-gguf","repo":"example/Qwen-GGUF","path":"/models/qwen.gguf"}`); code != 200 || decodeUnload(t, body).Status != "noop" {
		t.Errorf("a second unload is a noop, got %d %s", code, body)
	}
}

// A shard part of the model, or an adapter held on it, is a 409 that names
// what is in the way — and nothing is stopped.
func TestWorkerUnloadRefusesWhatItDoesNotOwn(t *testing.T) {
	sup := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	eng := &launchedStub{name: "vllm", sup: sup, process: "vllm-serve", model: "Qwen3-8B"}
	srv := &Server{Engine: eng, Supervisor: sup, Aliases: &Aliases{}}
	call := unloadServer(t, srv)
	for _, id := range []string{"vllm-serve", "s-qwen3-8b-rpc-0"} {
		if _, err := sup.Start(context.Background(), ProcessSpec{ID: id, Command: "sleep", Args: []string{"30"}}); err != nil {
			t.Fatal(err)
		}
	}
	srv.noteLaunch("vllm-serve", "Qwen3-8B", "example/Qwen3-8B")

	code, body := call(`{"id":"Qwen3-8B","repo":"example/Qwen3-8B"}`)
	if code != http.StatusConflict || !strings.Contains(body, "s-qwen3-8b-rpc-0") {
		t.Fatalf("a shard part here is 409 naming the process: %d %s", code, body)
	}
	if err := sup.Stop("s-qwen3-8b-rpc-0"); err != nil {
		t.Fatal(err)
	}

	st := srv.adapters()
	st.mu.Lock()
	st.held["Qwen3-8B:support"] = Adapter{Name: "support", Source: "example/lora"}
	st.mu.Unlock()
	code, body = call(`{"id":"Qwen3-8B","repo":"example/Qwen3-8B"}`)
	if code != http.StatusConflict || !strings.Contains(body, "Qwen3-8B:support") {
		t.Fatalf("a held adapter is 409 naming it: %d %s", code, body)
	}
	if _, ok := sup.Get("vllm-serve"); !ok {
		t.Fatal("a refusal must not stop the engine")
	}

	st.mu.Lock()
	delete(st.held, "Qwen3-8B:support")
	st.mu.Unlock()
	if code, body := call(`{"id":"Qwen3-8B","repo":"example/Qwen3-8B"}`); code != 200 || decodeUnload(t, body).Status != "unloaded" {
		t.Fatalf("with nothing in the way: %d %s", code, body)
	}
}

// An engine that can drop one model is asked to, by the engine's own name; a
// model it does not hold is a noop; its failure is a 502.
func TestWorkerUnloadAsksAnEngineThatCan(t *testing.T) {
	eng := &daemonStub{installed: []string{"llama3.2:3b", "qwen3:8b"}, resident: []string{"llama3.2:3b", "qwen3:8b"}}
	al := &Aliases{}
	al.Note("llama3.2:3b", "llama-3.2-3b")
	srv := &Server{Engine: eng, Aliases: al}
	call := unloadServer(t, srv)

	code, body := call(`{"id":"llama-3.2-3b","ollama_name":"llama3.2:3b","repo":"example/Llama"}`)
	if ans := decodeUnload(t, body); code != 200 || ans.Status != "unloaded" || ans.Model != "llama3.2:3b" {
		t.Fatalf("unload: %d %s", code, body)
	}
	if len(eng.unloaded) != 1 || eng.unloaded[0] != "llama3.2:3b" {
		t.Fatalf("the engine is asked by its own name for the model, got %v", eng.unloaded)
	}
	if al.ID("llama3.2:3b") != "llama-3.2-3b" {
		t.Error("the weights stay installed, so the alias stays: a later load is reported under the id")
	}
	if code, body := call(`{"id":"llama-3.2-3b","ollama_name":"llama3.2:3b"}`); code != 200 || decodeUnload(t, body).Status != "noop" {
		t.Errorf("not resident is a noop: %d %s", code, body)
	}

	eng.unloadErr = errors.New("ollama unload: 500 boom")
	if code, body := call(`{"id":"qwen3-8b","ollama_name":"qwen3:8b"}`); code != http.StatusBadGateway || !strings.Contains(body, "boom") {
		t.Errorf("an engine failure is 502 with the engine's words: %d %s", code, body)
	}
}

// An engine the worker did not start and that cannot unload is 501 in the
// shape /v1/model/sleep uses — never a pretended "unloaded"; a model it does
// not list is still a noop.
func TestWorkerUnloadSaysWhenItCannot(t *testing.T) {
	srv := &Server{Engine: &externalStub{models: []string{"qwen3-8b"}}, Aliases: &Aliases{}}
	call := unloadServer(t, srv)

	code, body := call(`{"id":"qwen3-8b","repo":"example/Qwen3-8B"}`)
	ans := decodeUnload(t, body)
	if code != http.StatusNotImplemented || ans.Status != "unsupported" || ans.Engine != "vllm" || ans.Reason == "" {
		t.Fatalf("501 unsupported with the engine and a reason: %d %s", code, body)
	}
	if code, body := call(`{"id":"absent","repo":"example/Absent"}`); code != 200 || decodeUnload(t, body).Status != "noop" {
		t.Errorf("not listed is a noop: %d %s", code, body)
	}
	if code, _ := call(`{"repo":"example/Qwen3-8B"}`); code != http.StatusBadRequest {
		t.Errorf("id is required: %d", code)
	}
	if code, _ := call(`{not json`); code != http.StatusBadRequest {
		t.Errorf("a malformed body is 400: %d", code)
	}
}

// A start-up adapter loader waiting for a launch gives up once that launch is
// unloaded or replaced, rather than loading into whatever runs next.
func TestAdapterLoaderEndsWithItsLaunch(t *testing.T) {
	srv := &Server{Engine: &externalStub{}, Aliases: &Aliases{}}
	gen := srv.launchGen.Add(1)
	srv.launchGen.Add(1) // the launch it waited on is gone
	done := make(chan struct{})
	go func() {
		srv.loadAdaptersWhenReady("base", []Adapter{{Name: "a", Source: "s"}}, gen)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the loader kept waiting for a launch that no longer exists")
	}
}
