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
	"github.com/opod-io/opod/internal/engines"
	_ "github.com/opod-io/opod/internal/engines/all" // the drivers a shipped binary links
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
		Engines  []EngineInfo    `json:"engines"`
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
	for _, k := range contractFeatureFloor {
		if !caps.Features[k] {
			t.Errorf("feature %q left the contract — feature keys are additive-only: a manager probes for it", k)
		}
	}
	assertContractEngines(t, caps.Features, caps.Engines)

	rec = httptest.NewRecorder()
	srv.adminVersion(rec, httptest.NewRequest(http.MethodGet, "/admin/v1/version", nil))
	var ver map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &ver); err != nil || ver["version"] == "" || ver["contract"] != ContractVersion {
		t.Fatalf("version: %v %s", err, rec.Body.String())
	}
}

// contractFeatureFloor is every feature key the contract has published. A key
// may be added (here too); one that disappears fails the build, because a
// manager that probes for it would silently stop using the mechanism.
var contractFeatureFloor = []string{
	"events_stream", "usage_stream", "loadz", "shards", "plan_file", "auth_file", "router_only_ready",
	"routing_weights", "model_revision", "adapters_runtime", "vram_budget", "stream_boot", "load_signals",
	"lora", "boot_id", "tls_listener", "pd_roles", "routing_load_aware", "worker_sleep", "policy_file",
	"shard_head", "ttft", "engines", "fetch_snapshot", "otlp_logs", "cache_prune", "gang_devices_per_rank",
	"node_drain",      // POST /admin/v1/nodes/{id}/drain|undrain, honoured by every picker
	"placement_drain", // a draining placement survives the worker's heartbeats
	"worker_unload",   // POST /v1/model/unload on a worker
}

// contractEngineNames is the engine half of the additive-only rule: every id
// and alias a manager could have been built against. A driver may be added and
// an alias may be added; dropping or renaming one of these breaks a plan that
// names it, at process launch, far from here — so it fails here instead.
var contractEngineNames = map[string]struct {
	aliases []string
	native  string
}{
	"llamacpp": {[]string{"llama-cpp", "llamacpp-rpc"}, "repo"},
	"mlx":      {[]string{"mlx-lm"}, "repo"},
	"ollama":   {nil, "ollama_name"},
	"sglang":   {[]string{"sgl"}, "repo"},
	"vllm":     {[]string{"tenstorrent", "tt", "tt-openai"}, "repo"},
}

// assertContractEngines: capabilities reports exactly the drivers linked into
// the binary, each under the name the registry resolves, and still carries
// every name the contract has ever published.
func assertContractEngines(t *testing.T, features map[string]bool, got []EngineInfo) {
	t.Helper()
	if !features["engines"] {
		t.Error(`feature "engines" is not advertised, so a manager will not read the engine list`)
	}
	byID := map[string]EngineInfo{}
	for _, e := range got {
		if e.ID == "" || engines.Canonical(e.ID) != e.ID {
			t.Errorf("engine %+v: id is not a canonical registry name", e)
		}
		for _, a := range e.Aliases {
			if engines.Canonical(a) != e.ID {
				t.Errorf("engine %s: alias %q resolves to %q", e.ID, a, engines.Canonical(a))
			}
		}
		switch e.Native {
		case "id", "ollama_name", "repo", "path":
		default:
			t.Errorf("engine %s: native = %q, want a catalog source field", e.ID, e.Native)
		}
		byID[e.ID] = e
	}
	if linked := engines.Names(); len(byID) != len(linked) {
		t.Errorf("capabilities lists %d engines, the binary links %d (%v)", len(byID), len(linked), linked)
	}
	for id, want := range contractEngineNames {
		e, ok := byID[id]
		if !ok {
			t.Errorf("engine %q left the contract — engine ids are additive-only", id)
			continue
		}
		have := map[string]bool{}
		for _, a := range e.Aliases {
			have[a] = true
		}
		for _, a := range want.aliases {
			if !have[a] {
				t.Errorf("engine %s: alias %q left the contract — aliases are additive-only", id, a)
			}
		}
		if e.Native != want.native {
			t.Errorf("engine %s: native = %q, was %q — a manager resolves model names by it", id, e.Native, want.native)
		}
	}
}
