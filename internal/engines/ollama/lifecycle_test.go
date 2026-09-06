package ollama_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/opod-io/opod/internal/engines/ollama"
)

// TestResident verifies /api/ps parsing — name + RAM/VRAM byte fields are
// what the lifecycle manager's admission math runs on.
func TestResident(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ps" || r.Method != http.MethodGet {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"models":[
			{"name":"llama3.2:3b","size":3825819519,"size_vram":3825819519},
			{"name":"nomic-embed-text:latest","size":293248000,"size_vram":0}
		]}`))
	}))
	defer srv.Close()

	got, err := ollama.New(srv.URL).Resident(context.Background())
	if err != nil {
		t.Fatalf("Resident: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d models, want 2", len(got))
	}
	if got[0].Name != "llama3.2:3b" || got[0].SizeBytes != 3825819519 || got[0].VRAMBytes != 3825819519 {
		t.Errorf("model[0] = %+v", got[0])
	}
	if got[1].SizeBytes != 293248000 || got[1].VRAMBytes != 0 {
		t.Errorf("model[1] = %+v", got[1])
	}
}

// TestLoadKeepAlive verifies the load request shape: no prompt, and
// keep_alive=-1 ONLY when pinning (unpinned loads must leave the daemon's
// default TTL intact by omitting the field).
func TestLoadKeepAlive(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		bodies = append(bodies, m)
		_, _ = w.Write([]byte(`{"done":true}`))
	}))
	defer srv.Close()

	o := ollama.New(srv.URL)
	if err := o.Load(context.Background(), "llama3.2:3b", false); err != nil {
		t.Fatalf("Load unpinned: %v", err)
	}
	if err := o.Load(context.Background(), "llama3.2:3b", true); err != nil {
		t.Fatalf("Load pinned: %v", err)
	}

	if _, hasKA := bodies[0]["keep_alive"]; hasKA {
		t.Errorf("unpinned load must omit keep_alive, got %v", bodies[0])
	}
	if _, hasPrompt := bodies[0]["prompt"]; hasPrompt {
		t.Errorf("load must not send a prompt, got %v", bodies[0])
	}
	if ka, ok := bodies[1]["keep_alive"].(float64); !ok || ka != -1 {
		t.Errorf("pinned load must send keep_alive=-1, got %v", bodies[1])
	}
}
