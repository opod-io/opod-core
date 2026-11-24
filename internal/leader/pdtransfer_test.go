package leader

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"

	"github.com/opod-io/opod-core/internal/config"
	"github.com/opod-io/opod-core/internal/store"
)

// pdSeries is every series of the pd transfer families on /metrics, keyed
// "name{node,stage}".
func pdSeries(t *testing.T, h http.Handler) map[string]float64 {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	p := expfmt.NewTextParser(model.LegacyValidation)
	fams, err := p.TextToMetricFamilies(strings.NewReader(rec.Body.String()))
	if err != nil {
		t.Fatalf("parse /metrics: %v", err)
	}
	out := map[string]float64{}
	for _, name := range []string{"opod_pd_transfer_bytes_total", "opod_pd_transfer_seconds_total", "opod_pd_transfers_total", "opod_pd_kv_fetch_requests"} {
		f := fams[name]
		if f == nil {
			continue
		}
		for _, m := range f.GetMetric() {
			key := name + "{"
			for _, l := range m.GetLabel() {
				key += l.GetValue() + ","
			}
			v := m.GetGauge().GetValue()
			if c := m.GetCounter(); c != nil {
				v = c.GetValue()
			}
			out[strings.TrimSuffix(key, ",")+"}"] = v
		}
	}
	return out
}

// PLAN T28.13: the KV transfer figures a worker's engine states reach the
// leader's /metrics per worker; a worker whose engine states none has no
// series at all (unknown is absent, never 0), and a forgotten worker's series
// go with it, so the series are bounded by the leader's workers.
func TestPDTransferFiguresAreWhatTheEngineStated(t *testing.T) {
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
	for _, id := range []string{"wd", "wp"} {
		if err := st.Nodes().Upsert(ctx, store.Node{ID: id, Hostname: id, State: "ready", LastHeartbeat: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	for id, load := range map[string]string{
		"wd": `{"kv_used_pct":10,"kv_xfer_bytes_total":4194304,"kv_xfer_seconds_total":0.375,"kv_xfers_total":4,"kv_fetch_in_progress":6,"kv_fetch_loaded":0}`,
		"wp": `{"kv_used_pct":10}`, // no connector figures: nothing is stated
	} {
		rec := httptest.NewRecorder()
		srv.heartbeatNode(rec, httptest.NewRequest(http.MethodPost, "/admin/v1/nodes/heartbeat",
			strings.NewReader(`{"id":"`+id+`","loaded_models":["m"],"load":`+load+`}`)))
		if rec.Code != http.StatusOK {
			t.Fatalf("heartbeat %s: %d %s", id, rec.Code, rec.Body.String())
		}
	}

	got := pdSeries(t, srv.probeRoutes())
	want := map[string]float64{
		"opod_pd_transfer_bytes_total{wd}":          4 << 20,
		"opod_pd_transfer_seconds_total{wd}":        0.375,
		"opod_pd_transfers_total{wd}":               4,
		"opod_pd_kv_fetch_requests{wd,in_progress}": 6,
		"opod_pd_kv_fetch_requests{wd,loaded}":      0, // stated by the engine: a true zero
	}
	if len(got) != len(want) {
		t.Errorf("series = %v, want exactly %v (the worker that stated nothing has none; nor does an unstated stage)", got, want)
	}
	for k, v := range want {
		if g, ok := got[k]; !ok || g != v {
			t.Errorf("%s = %v (present %v), want %v", k, g, ok, v)
		}
	}

	srv.forgetNode(ctx, "wd")
	if got := pdSeries(t, srv.probeRoutes()); len(got) != 0 {
		t.Errorf("a forgotten worker keeps no series: %v", got)
	}
}
