package controlplane

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/store"
)

// TestLeaderContract: every route in LeaderContract is registered on the
// real router with the listed method. This is the build-time guard for the
// additive-only admin surface — deleting or renaming one of those routes
// fails here before it can fail in a manager talking to a shipped binary.
func TestLeaderContract(t *testing.T) {
	cfg := config.Default()
	cfg.Listen = ":0"
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := NewServer(cfg, st, &deadEngine{&stubLeaderEngine{}}, nil, log, nil)

	mux, ok := srv.routes().(chi.Router)
	if !ok {
		t.Fatal("routes() must return the chi router so the contract can be walked")
	}
	have := map[string]bool{}
	err = chi.Walk(mux, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		// chi reports mounted patterns with a trailing "/*" for sub-routers
		// and keeps placeholders verbatim; normalise only the slash noise.
		route = strings.TrimSuffix(route, "/")
		if route == "" {
			route = "/"
		}
		have[method+" "+route] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range LeaderContract {
		if !have[r.Method+" "+r.Path] {
			t.Errorf("contract route missing from the router: %s %s (LeaderContract is additive-only — restore it)", r.Method, r.Path)
		}
	}

	// The two discovery routes must describe the same list this test enforces.
	rec := httptest.NewRecorder()
	srv.adminCapabilities(rec, httptest.NewRequest(http.MethodGet, "/admin/v1/capabilities", nil))
	var caps struct {
		Contract string          `json:"contract"`
		Routes   []ContractRoute `json:"routes"`
		Features map[string]bool `json:"features"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &caps); err != nil || caps.Contract != ContractVersion {
		t.Fatalf("capabilities: %v %s", err, rec.Body.String())
	}
	if len(caps.Routes) != len(LeaderContract) {
		t.Errorf("capabilities lists %d routes, contract has %d", len(caps.Routes), len(LeaderContract))
	}
	for k, v := range caps.Features {
		if !v {
			t.Errorf("feature %q advertised false — remove the key instead", k)
		}
	}
	rec = httptest.NewRecorder()
	srv.adminVersion(rec, httptest.NewRequest(http.MethodGet, "/admin/v1/version", nil))
	var ver map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &ver); err != nil || ver["version"] == "" || ver["contract"] != ContractVersion {
		t.Fatalf("version: %v %s", err, rec.Body.String())
	}
}
