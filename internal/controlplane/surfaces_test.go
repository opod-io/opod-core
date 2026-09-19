package controlplane

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/store"
)

// contractedServer is a leader the way an external manager runs one
// (OPOD_MANAGED=1). The product surfaces this test asserts the absence of are
// not switched off — they are not in the binary (ADR-022).
func contractedServer(t *testing.T) (*Server, string) {
	t.Helper()
	cfg := config.Default()
	cfg.Listen = ":0"
	cfg.Surfaces = config.SurfacesConfig{Managed: true}
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	plain, rec, err := auth.Generate("test-admin", "admin", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.APIKeys().Create(t.Context(), rec); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewServer(cfg, st, &deadEngine{&stubLeaderEngine{}}, nil, log, nil), plain
}

// TestSurfacesOff: a managed leader serves no product surface and the whole
// frozen contract (contract.go).
func TestSurfacesOff(t *testing.T) {
	srv, key := contractedServer(t)
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	get := func(path string, withKey bool) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		if withKey {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	post := func(path string) int {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// off
	// ADR-022: no dashboard, no bootstrap-key, no connect/invite in core at all
	for _, p := range []string{"/", "/admin/v1/bootstrap-key", "/admin/v1/connect/clients"} {
		if c, _ := get(p, true); c != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404 (removed from core)", p, c)
		}
	}
	if c, _ := get("/admin/v1/callbacks", true); c != http.StatusNotFound {
		t.Errorf("GET /admin/v1/callbacks = %d, want 404 (callbacks left core, ADR-022)", c)
	}
	for _, p := range []string{"/v1/messages", "/v1/messages/count_tokens", "/v1/audio/speech", "/v1/audio/transcriptions", "/v1/rerank"} {
		if c := post(p); c != http.StatusNotFound {
			t.Errorf("POST %s with protocols=openai = %d, want 404", p, c)
		}
	}

	// still on: the contract
	mux := srv.routes().(chi.Router)
	have := map[string]bool{}
	_ = chi.Walk(mux, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		have[method+" "+strings.TrimSuffix(route, "/")] = true
		return nil
	})
	for _, r := range LeaderContract {
		if !have[r.Method+" "+r.Path] {
			t.Errorf("contract route missing on a contracted leader: %s %s", r.Method, r.Path)
		}
	}
	if c, body := get("/admin/v1/capabilities", true); c != http.StatusOK || !strings.Contains(body, `"contract":"v1"`) {
		t.Errorf("capabilities on a contracted leader: %d %s", c, body)
	}
	if c, _ := get("/healthz", false); c != http.StatusOK {
		t.Errorf("healthz: %d", c)
	}
	if c, _ := get("/metrics", false); c != http.StatusOK {
		t.Errorf("metrics: %d", c)
	}
	if c, _ := get("/v1/models", true); c != http.StatusOK {
		t.Errorf("/v1/models with the admin key: %d", c)
	}
}

// TestStandaloneIsTheDefault: `opod up` with no configuration is not managed.
func TestStandaloneIsTheDefault(t *testing.T) {
	if cfg := config.Default(); cfg.Surfaces.Managed {
		t.Fatalf("a default config must be standalone: %+v", cfg.Surfaces)
	}
}
