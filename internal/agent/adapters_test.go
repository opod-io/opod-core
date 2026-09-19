package agent

// R9.2: adapters as variants of one identity, against a fake vLLM.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/engines"
)

// fakeVLLM answers the two routes the adapter code uses and the model list.
type fakeVLLM struct {
	srv    *httptest.Server
	loaded []string
	base   string
}

func newFakeVLLM(t *testing.T, base string) *fakeVLLM {
	t.Helper()
	f := &fakeVLLM{base: base}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/load_lora_adapter":
			var req map[string]string
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req["lora_name"] == "" || req["lora_path"] == "" {
				http.Error(w, "lora_name and lora_path required", 400)
				return
			}
			f.loaded = append(f.loaded, req["lora_name"])
			w.WriteHeader(200)
		case "/v1/unload_lora_adapter":
			var req map[string]string
			_ = json.NewDecoder(r.Body).Decode(&req)
			kept := f.loaded[:0]
			for _, n := range f.loaded {
				if n != req["lora_name"] {
					kept = append(kept, n)
				}
			}
			f.loaded = kept
			w.WriteHeader(200)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

type loraEngine struct {
	engines.Engine
	f *fakeVLLM
}

func (e *loraEngine) Name() string     { return "vllm" }
func (e *loraEngine) Endpoint() string { return e.f.srv.URL }
func (e *loraEngine) List(context.Context) ([]string, error) {
	return append([]string{e.f.base}, e.f.loaded...), nil
}

func TestAdapters_LoadServesUnderTheBaseIdentity(t *testing.T) {
	f := newFakeVLLM(t, "mimo-7b")
	al := &Aliases{}
	srv := &Server{Token: "sk", Engine: &loraEngine{f: f}, Aliases: al}
	if err := srv.loadAdapter(context.Background(), "mimo-7b", Adapter{Name: "sql", Source: "org/mimo-sql-lora"}); err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(f.loaded) != 1 || f.loaded[0] != "sql" {
		t.Fatalf("vLLM asked to load the adapter by its name: %v", f.loaded)
	}
	// The heartbeat reports the suffixed id; the proxy rewrites it back.
	got := al.Resolve([]string{"mimo-7b", "sql"})
	if len(got) != 2 || got[1] != "mimo-7b:sql" {
		t.Fatalf("loaded ids: %v", got)
	}
	if al.Native("mimo-7b:sql") != "sql" || al.Native("mimo-7b") != "mimo-7b" {
		t.Fatalf("native names: %q %q", al.Native("mimo-7b:sql"), al.Native("mimo-7b"))
	}
	if err := srv.unloadAdapter(context.Background(), "mimo-7b", "sql"); err != nil {
		t.Fatalf("unload: %v", err)
	}
	if len(f.loaded) != 0 || al.Native("mimo-7b:sql") != "mimo-7b:sql" {
		t.Fatalf("unloaded: %v %q", f.loaded, al.Native("mimo-7b:sql"))
	}
	// Refusals: a bad name, a non-vLLM engine.
	if err := srv.loadAdapter(context.Background(), "mimo-7b", Adapter{Name: "Bad Name", Source: "x"}); err == nil {
		t.Fatal("a bad adapter name must be refused")
	}
	srv.Engine = plainEngine{}
	if err := srv.loadAdapter(context.Background(), "m", Adapter{Name: "a", Source: "x"}); err == nil || !strings.Contains(err.Error(), "vLLM") {
		t.Fatalf("llama.cpp has no LoRA slots: %v", err)
	}
}

func TestAdapters_RoutesAndEnv(t *testing.T) {
	f := newFakeVLLM(t, "mimo-7b")
	srv := &Server{Token: "sk", Engine: &loraEngine{f: f}, Aliases: &Aliases{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/adapters", srv.auth(srv.adaptersList))
	mux.HandleFunc("/v1/adapters/load", srv.auth(srv.adaptersLoad))
	mux.HandleFunc("/v1/adapters/unload", srv.auth(srv.adaptersUnload))
	ts := httptest.NewServer(mux)
	defer ts.Close()
	call := func(method, path, body string) (int, string) {
		r, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer sk")
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b := make([]byte, 512)
		n, _ := resp.Body.Read(b)
		return resp.StatusCode, string(b[:n])
	}
	if code, body := call("POST", "/v1/adapters/load", `{"base":"mimo-7b","name":"sql","source":"org/lora"}`); code != 200 || !strings.Contains(body, "mimo-7b:sql") {
		t.Fatalf("load: %d %s", code, body)
	}
	if code, body := call("GET", "/v1/adapters", ""); code != 200 || !strings.Contains(body, `"id":"mimo-7b:sql"`) {
		t.Fatalf("list: %d %s", code, body)
	}
	if code, _ := call("POST", "/v1/adapters/unload", `{"base":"mimo-7b","name":"sql"}`); code != 200 {
		t.Fatalf("unload: %d", code)
	}
	if code, _ := call("POST", "/v1/adapters/load", `{"name":"sql"}`); code != 400 {
		t.Fatalf("missing base: %d", code)
	}

	got, err := ParseAdapters(`[{"name":"sql","source":"org/lora"},{"name":"chat","source":"/data/models/chat-lora"}]`)
	if err != nil || len(got) != 2 || got[1].Source != "/data/models/chat-lora" {
		t.Fatalf("env: %v %v", got, err)
	}
	if vllmLoRAArgs(len(got), 0) != "--enable-lora --max-loras 4" || vllmLoRAArgs(0, 0) != "" {
		t.Fatalf("lora args: %q %q", vllmLoRAArgs(len(got), 0), vllmLoRAArgs(0, 0))
	}
	if _, err := ParseAdapters(`[{"name":"Bad","source":"x"}]`); err == nil {
		t.Fatal("a bad name in the env is an error, not a loaded adapter")
	}
}

// TestLoRARankSizesTheEngine: vLLM's --max-lora-rank defaults to 16 and refuses
// a bigger adapter when it is loaded, so the engine must be started for the
// largest rank the set declares. A rank at or under the default changes nothing.
func TestLoRARankSizesTheEngine(t *testing.T) {
	as, err := ParseAdapters(`[{"name":"a","source":"org/a","rank":8},{"name":"b","source":"org/b","rank":64}]`)
	if err != nil {
		t.Fatal(err)
	}
	if got := MaxAdapterRank(as); got != 64 {
		t.Fatalf("largest rank in the set: %d", got)
	}
	if got := vllmLoRAArgs(len(as), MaxAdapterRank(as)); got != "--enable-lora --max-loras 4 --max-lora-rank 64" {
		t.Fatalf("args for a rank-64 adapter: %q", got)
	}
	// At or below vLLM's own default the flag is not passed at all.
	if got := vllmLoRAArgs(2, 16); got != "--enable-lora --max-loras 4" {
		t.Fatalf("rank 16 is the engine's default: %q", got)
	}
	// A rank between two accepted sizes rounds up; an absurd one lands on the largest.
	for rank, want := range map[int]int{17: 32, 33: 64, 200: 256, 999: 256} {
		if got := loRARankFor(rank); got != want {
			t.Errorf("loRARankFor(%d) = %d, want %d", rank, got, want)
		}
	}
	// A rank that travelled with the adapter survives parsing.
	if as[1].Rank != 64 {
		t.Errorf("rank lost in ParseAdapters: %+v", as[1])
	}
}

// vLLM sizes its LoRA slots when it starts. A live add the running engine
// cannot take is refused by the worker — 409, naming the adapter's rank, the
// rank the engine was started for and the remedy — and never reaches the
// engine, whose own error names neither number.
func TestLiveAdapterTheLaunchCannotTakeIsRefused(t *testing.T) {
	f := newFakeVLLM(t, "mimo-7b")
	newServer := func(atStart []Adapter, flags EngineFlags) (*Server, func(body string) (int, string)) {
		srv := &Server{Token: "sk", Engine: &loraEngine{f: f}, Aliases: &Aliases{}, Adapters: atStart, EngineFlags: flags}
		mux := http.NewServeMux()
		mux.HandleFunc("/v1/adapters/load", srv.auth(srv.adaptersLoad))
		ts := httptest.NewServer(mux)
		t.Cleanup(ts.Close)
		return srv, func(body string) (int, string) {
			r, _ := http.NewRequest("POST", ts.URL+"/v1/adapters/load", strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer sk")
			resp, err := http.DefaultClient.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			b := make([]byte, 1024)
			n, _ := resp.Body.Read(b)
			return resp.StatusCode, string(b[:n])
		}
	}

	// Started with one rank-8 adapter: --enable-lora, vLLM's default max rank.
	srv, load := newServer([]Adapter{{Name: "sql", Source: "org/sql", Rank: 8}}, nil)
	srv.noteLoRALaunch()
	code, body := load(`{"base":"mimo-7b","name":"big","source":"org/big","rank":64}`)
	if code != http.StatusConflict {
		t.Fatalf("rank 64 on an engine started for 16: %d %s, want 409", code, body)
	}
	for _, want := range []string{`"big"`, "rank 64", "--max-lora-rank 16", "restart", "OPOD_ADAPTERS"} {
		if !strings.Contains(body, want) {
			t.Errorf("the refusal does not say %q: %s", want, body)
		}
	}
	if len(f.loaded) != 0 {
		t.Fatalf("a refused adapter reached the engine: %v", f.loaded)
	}
	// At the launched rank, and with no rank stated, the engine is asked.
	if code, body := load(`{"base":"mimo-7b","name":"fits","source":"org/fits","rank":16}`); code != 200 {
		t.Fatalf("rank 16 on an engine started for 16: %d %s", code, body)
	}
	if code, body := load(`{"base":"mimo-7b","name":"unstated","source":"org/u"}`); code != 200 {
		t.Fatalf("no rank stated — the engine answers: %d %s", code, body)
	}
	if code, _ := load(`{"base":"mimo-7b","name":"neg","source":"org/n","rank":-1}`); code != http.StatusBadRequest {
		t.Fatalf("a negative rank: %d, want 400", code)
	}

	// Started for a rank-64 set: 64 fits, 65 does not.
	srv, load = newServer([]Adapter{{Name: "a", Source: "org/a", Rank: 40}}, nil)
	srv.noteLoRALaunch()
	if code, body := load(`{"base":"mimo-7b","name":"r64","source":"org/r","rank":64}`); code != 200 {
		t.Fatalf("rank 64 on an engine started with --max-lora-rank 64: %d %s", code, body)
	}
	if code, body := load(`{"base":"mimo-7b","name":"r65","source":"org/r","rank":65}`); code != http.StatusConflict || !strings.Contains(body, "--max-lora-rank 64") {
		t.Fatalf("rank 65 on an engine started for 64: %d %s", code, body)
	}

	// Started with no adapter at all: no --enable-lora, nothing can be added live.
	srv, load = newServer(nil, nil)
	srv.noteLoRALaunch()
	before := len(f.loaded)
	if code, body := load(`{"base":"mimo-7b","name":"late","source":"org/late"}`); code != http.StatusConflict || !strings.Contains(body, "without LoRA slots") {
		t.Fatalf("an engine started without LoRA slots: %d %s", code, body)
	}
	if len(f.loaded) != before {
		t.Fatal("the engine was asked although it has no LoRA slots")
	}

	// What the worker does not know, it does not refuse: an engine it did not
	// launch, and one whose operator sized LoRA in the extra flags.
	_, load = newServer(nil, nil) // no launch recorded — an external vLLM
	if code, body := load(`{"base":"mimo-7b","name":"ext","source":"org/e","rank":128}`); code != 200 {
		t.Fatalf("an engine this worker did not launch answers for itself: %d %s", code, body)
	}
	srv, load = newServer(nil, EngineFlags{"extra": "--enable-lora --max-lora-rank 128"})
	srv.noteLoRALaunch()
	if code, body := load(`{"base":"mimo-7b","name":"op","source":"org/o","rank":128}`); code != 200 {
		t.Fatalf("an operator-sized engine answers for itself: %d %s", code, body)
	}
}
