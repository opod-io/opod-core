package leader

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"

	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/store"
)

// scrapeMetrics reads /metrics from h and parses it the way a scraper does.
func scrapeMetrics(t *testing.T, h http.Handler) map[string]float64 {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics: %d", rec.Code)
	}
	p := expfmt.NewTextParser(model.LegacyValidation) // the vllm: names are legacy-valid: colons are allowed
	fams, err := p.TextToMetricFamilies(strings.NewReader(rec.Body.String()))
	if err != nil {
		t.Fatalf("parse /metrics: %v", err)
	}
	out := map[string]float64{}
	for name, f := range fams {
		for _, m := range f.GetMetric() {
			switch {
			case m.GetGauge() != nil:
				out[name] = m.GetGauge().GetValue()
			default:
				out[name] = 0
			}
		}
	}
	return out
}

// TestVLLMAliasContract is the build-time guard ADR-083 asks for: an inference
// pool whose endpoint picker reads vLLM's names routes to this leader, so an
// alias that disappears breaks a customer's gateway without a word. It must be
// on the probe port AND the main listener, beside the opod_* names.
func TestVLLMAliasContract(t *testing.T) {
	cfg := config.Default()
	cfg.Listen = ":0"
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := NewServer(cfg, st, &deadEngine{&stubLeaderEngine{}}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)

	for name, h := range map[string]http.Handler{"probe port": srv.probeRoutes(), "main listener": srv.routes()} {
		got := scrapeMetrics(t, h)
		for _, alias := range VLLMAliasNames {
			if _, ok := got[alias]; !ok {
				t.Errorf("%s: alias %s is missing from /metrics — an inference pool reading it stops routing here (ADR-083)", name, alias)
			}
		}
		// ObserveRequest has not run here, so check a name registered at init.
		if _, ok := got["opod_router_cooldowns_active"]; !ok {
			t.Errorf("%s: the opod_* names are gone; the aliases go beside them, never instead", name)
		}
	}
}

// The aliases carry what the leader knows: summed queue, in-flight less that
// queue, and the MEAN KV use as a fraction — not the maximum /loadz carries.
func TestVLLMAliasValues(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.Listen = ":0"
	cfg.Router.HeartbeatMaxAgeSeconds = 30
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := NewServer(cfg, st, &deadEngine{&stubLeaderEngine{}}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	for _, id := range []string{"w1", "w2"} {
		if err := st.Nodes().Upsert(ctx, store.Node{ID: id, Hostname: id, State: "ready", LastHeartbeat: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	for id, load := range map[string]string{
		"w1": `{"kv_used_pct":40,"queue_depth":1}`,
		"w2": `{"kv_used_pct":90,"queue_depth":2}`,
	} {
		rec := httptest.NewRecorder()
		srv.heartbeatNode(rec, httptest.NewRequest(http.MethodPost, "/admin/v1/nodes/heartbeat",
			strings.NewReader(`{"id":"`+id+`","loaded_models":["m"],"load":`+load+`}`)))
		if rec.Code != http.StatusOK {
			t.Fatalf("heartbeat %s: %d %s", id, rec.Code, rec.Body.String())
		}
	}
	atomic.StoreInt64(&srv.load.inFlight, 7)

	got := scrapeMetrics(t, srv.probeRoutes())
	if got["vllm:num_requests_waiting"] != 3 {
		t.Errorf("waiting = %v, want 3 (1 + 2)", got["vllm:num_requests_waiting"])
	}
	if got["vllm:num_requests_running"] != 4 {
		t.Errorf("running = %v, want 4 (7 in flight − 3 queued)", got["vllm:num_requests_running"])
	}
	if kv := got["vllm:kv_cache_usage_perc"]; kv < 0.649 || kv > 0.651 {
		t.Errorf("kv = %v, want 0.65 (mean of 0.40 and 0.90, a fraction)", kv)
	}

	// More queued than in flight (a sample a heartbeat old) is never negative.
	atomic.StoreInt64(&srv.load.inFlight, 1)
	if got := scrapeMetrics(t, srv.probeRoutes()); got["vllm:num_requests_running"] != 0 {
		t.Errorf("running = %v, want 0", got["vllm:num_requests_running"])
	}
}
