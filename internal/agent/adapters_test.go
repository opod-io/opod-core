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

	t.Setenv("OPOD_ADAPTERS", `[{"name":"sql","source":"org/lora"},{"name":"chat","source":"/data/models/chat-lora"}]`)
	got, err := adaptersFromEnv()
	if err != nil || len(got) != 2 || got[1].Source != "/data/models/chat-lora" {
		t.Fatalf("env: %v %v", got, err)
	}
	if vllmLoRAArgs(len(got)) != "--enable-lora --max-loras 4" || vllmLoRAArgs(0) != "" {
		t.Fatalf("lora args: %q %q", vllmLoRAArgs(len(got)), vllmLoRAArgs(0))
	}
	t.Setenv("OPOD_ADAPTERS", `[{"name":"Bad","source":"x"}]`)
	if _, err := adaptersFromEnv(); err == nil {
		t.Fatal("a bad name in the env is an error, not a loaded adapter")
	}
}
