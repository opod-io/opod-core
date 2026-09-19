package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/models"
	"github.com/opod-io/opod/internal/store"
)

// A leader-driven load (`opod model add <id> --node <n>`) carries the catalog
// entry's source.file, so a worker given a repository that holds several GGUF
// files serves the one the entry names; an entry without one omits the key,
// as the typed node protocol declares it (`json:"file,omitempty"`).
func TestPlaceOnNodesSendsTheCatalogFile(t *testing.T) {
	var bodies []map[string]any
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/model/load" {
			http.NotFound(w, r)
			return
		}
		var got map[string]any
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("load body: %v", err)
		}
		bodies = append(bodies, got)
		_, _ = io.WriteString(w, `{"status":"ready"}`)
	}))
	defer worker.Close()

	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.Nodes().Upsert(ctx, store.Node{ID: "w1", Address: strings.TrimPrefix(worker.URL, "http://"),
		WorkerToken: "tok", State: store.NodeStateReady, LastHeartbeat: time.Now()}); err != nil {
		t.Fatal(err)
	}
	o := New(st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir())

	multi := models.Entry{ID: "qwen-gguf", Source: models.SourceSpec{Type: "huggingface", Repo: "example/Qwen-GGUF", File: "qwen-q4_k_m.gguf"}}
	if err := o.PlaceOnNodes(ctx, multi, []string{"w1"}, true, false); err != nil {
		t.Fatal(err)
	}
	whole := models.Entry{ID: "qwen-awq", Source: models.SourceSpec{Type: "huggingface", Repo: "example/Qwen-AWQ"}}
	if err := o.PlaceOnNodes(ctx, whole, []string{"w1"}, false, false); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 {
		t.Fatalf("the worker saw %d loads, want 2", len(bodies))
	}
	if got := bodies[0]["file"]; got != "qwen-q4_k_m.gguf" {
		t.Errorf("multi-file repository: file = %v, want the catalog entry's source.file (body %v)", got, bodies[0])
	}
	if bodies[0]["id"] != "qwen-gguf" || bodies[0]["repo"] != "example/Qwen-GGUF" || bodies[0]["pin"] != true {
		t.Errorf("the other source fields must still travel: %v", bodies[0])
	}
	if _, present := bodies[1]["file"]; present {
		t.Errorf("an entry with no source.file must omit the key, got %v", bodies[1])
	}
	// The keys a worker reads unconditionally are always present, empty or not.
	for _, k := range []string{"id", "ollama_name", "repo", "path", "pin"} {
		if _, ok := bodies[1][k]; !ok {
			t.Errorf("key %q missing from the load body: %v", k, bodies[1])
		}
	}
}

// UnloadFromNode is the leader's half of /v1/model/unload: the load body's
// source fields without file and pin, HMAC-signed, and each of the worker's
// answers kept apart — done, nothing to do, cannot by design, failed.
func TestUnloadFromNodeReadsEveryAnswer(t *testing.T) {
	var (
		code   = http.StatusOK
		answer = `{"status":"unloaded","model":"example/Qwen-GGUF"}`
		bodies []map[string]any
		signed []bool
	)
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/model/unload" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var got map[string]any
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("unload body: %v", err)
		}
		bodies = append(bodies, got)
		signed = append(signed, r.Header.Get("X-Opod-Auth") != "")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, answer)
	}))
	defer worker.Close()

	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	// A drained worker takes no new work and is still unloaded from.
	if err := st.Nodes().Upsert(ctx, store.Node{ID: "w1", Hostname: "node-a", Address: strings.TrimPrefix(worker.URL, "http://"),
		WorkerToken: "tok", State: store.NodeStateDraining, LastHeartbeat: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := st.Nodes().Upsert(ctx, store.Node{ID: "local", State: store.NodeStateReady, LastHeartbeat: time.Now()}); err != nil {
		t.Fatal(err)
	}
	o := New(st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir())
	entry := models.Entry{ID: "qwen-gguf", Source: models.SourceSpec{Type: "huggingface", Repo: "example/Qwen-GGUF",
		File: "qwen-q4_k_m.gguf", OllamaName: "qwen:7b", Path: "/models/qwen.gguf"}}

	res, err := o.UnloadFromNode(ctx, entry, "w1")
	if err != nil || !res.Unloaded || res.Model != "example/Qwen-GGUF" {
		t.Fatalf("unloaded: %+v, %v", res, err)
	}
	want := map[string]any{"id": "qwen-gguf", "ollama_name": "qwen:7b", "repo": "example/Qwen-GGUF", "path": "/models/qwen.gguf"}
	if len(bodies) != 1 || len(bodies[0]) != len(want) {
		t.Fatalf("the body is the load body's source fields without file and pin, got %v", bodies)
	}
	for k, v := range want {
		if bodies[0][k] != v {
			t.Errorf("body[%q] = %v, want %v", k, bodies[0][k], v)
		}
	}
	if !signed[0] {
		t.Error("the call must carry the HMAC header, like /v1/model/load")
	}

	answer = `{"status":"noop","model":"example/Qwen-GGUF","reason":"not resident in this worker's engine"}`
	if res, err := o.UnloadFromNode(ctx, entry, "node-a"); err != nil || res.Unloaded || res.Reason == "" {
		t.Errorf("noop, by hostname: %+v, %v", res, err)
	}

	for _, c := range []struct {
		code   int
		answer string
		want   string
	}{
		{http.StatusConflict, "model \"qwen-gguf\" runs here as part of a sharded placement (process s-qwen-gguf-rpc-0)", "s-qwen-gguf-rpc-0"},
		{http.StatusNotImplemented, `{"status":"unsupported","engine":"vllm","reason":"the engine cannot unload a model"}`, "engine vllm: the engine cannot unload a model"},
		{http.StatusBadGateway, "unload: ollama 500", "ollama 500"},
		{http.StatusNotFound, "404 page not found", "predates"},
	} {
		code, answer = c.code, c.answer
		_, err := o.UnloadFromNode(ctx, entry, "w1")
		var refusal *WorkerRefusal
		if !errors.As(err, &refusal) || refusal.Code != c.code || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "w1") {
			t.Errorf("%d: got %v, want a WorkerRefusal with that code naming the node and %q", c.code, err, c.want)
		}
	}

	if _, err := o.UnloadFromNode(ctx, entry, "nobody"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("an unknown node: %v", err)
	}
	if _, err := o.UnloadFromNode(ctx, entry, "local"); err == nil || !strings.Contains(err.Error(), "not a worker") {
		t.Errorf("the leader's own row: %v", err)
	}
}
