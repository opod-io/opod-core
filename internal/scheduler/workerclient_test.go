package scheduler

import (
	"context"
	"encoding/json"
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
	if err := o.PlaceOnNodes(ctx, multi, []string{"w1"}, true); err != nil {
		t.Fatal(err)
	}
	whole := models.Entry{ID: "qwen-awq", Source: models.SourceSpec{Type: "huggingface", Repo: "example/Qwen-AWQ"}}
	if err := o.PlaceOnNodes(ctx, whole, []string{"w1"}, false); err != nil {
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
