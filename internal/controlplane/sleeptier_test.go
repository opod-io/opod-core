package controlplane

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

	"github.com/go-chi/chi/v5"

	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/store"
)

// Build item 13, leader side: a heartbeat that says "sleeping" turns the
// worker's placements into sleeping rows (not routable), /readyz answers
// 200 mode sleeping-workers instead of degraded, and the resume/sleep
// proxies reach the worker's agent with the worker token — a worker whose
// engine has no sleep mode answers 501, passed through unchanged.
func TestSleepTierPlacementsAndReadyz(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.Listen = ":0"
	cfg.Router.HeartbeatMaxAgeSeconds = 30
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := NewServer(cfg, st, &deadEngine{&stubLeaderEngine{}}, nil, log, nil)

	// a fake worker agent: sleep → 200, resume → 501 (engine without sleep mode)
	var seenAuth string
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/model/sleep":
			_, _ = w.Write([]byte(`{"status":"sleeping","engine":"vllm"}`))
		case "/v1/model/resume":
			w.WriteHeader(http.StatusNotImplemented)
			_, _ = w.Write([]byte(`{"status":"unsupported"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer worker.Close()
	if err := st.Nodes().Upsert(ctx, store.Node{ID: "w1", Hostname: "w1", State: "ready", Address: strings.TrimPrefix(worker.URL, "http://"), WorkerToken: "wtok", LastHeartbeat: time.Now()}); err != nil {
		t.Fatal(err)
	}
	heartbeat := func(body string) {
		rec := httptest.NewRecorder()
		srv.heartbeatNode(rec, httptest.NewRequest(http.MethodPost, "/admin/v1/nodes/heartbeat", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("heartbeat: %d %s", rec.Code, rec.Body.String())
		}
	}
	readyz := func() (int, string) {
		rec := httptest.NewRecorder()
		srv.readyz(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		mode, _ := body["mode"].(string)
		return rec.Code, mode
	}
	heartbeat(`{"id":"w1","loaded_models":["m"]}`)
	if code, mode := readyz(); code != 200 || mode != "router-only" {
		t.Fatalf("awake worker: %d/%s", code, mode)
	}
	heartbeat(`{"id":"w1","loaded_models":["m"],"sleeping":true}`)
	ps, _ := st.Placements().GetByNode(ctx, "w1")
	if len(ps) != 1 || ps[0].Status != PlacementSleeping {
		t.Fatalf("sleeping heartbeat must mark the placement sleeping: %+v", ps)
	}
	if srv.hasLivePlacement(ctx) {
		t.Fatal("a sleeping placement is not routable")
	}
	if code, mode := readyz(); code != 200 || mode != "sleeping-workers" {
		t.Fatalf("sleeping workers keep the leader ready: %d/%s", code, mode)
	}
	heartbeat(`{"id":"w1","loaded_models":["m"]}`)
	if code, mode := readyz(); code != 200 || mode != "router-only" {
		t.Fatalf("resumed worker: %d/%s", code, mode)
	}

	// the proxies
	call := func(action string) (int, string) {
		r := chi.NewRouter()
		r.Post("/nodes/{id}/sleep", srv.sleepWorker)
		r.Post("/nodes/{id}/resume", srv.resumeWorker)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/nodes/w1/"+action, nil))
		return rec.Code, rec.Body.String()
	}
	if code, body := call("sleep"); code != 200 || !strings.Contains(body, "sleeping") || seenAuth != "Bearer wtok" {
		t.Fatalf("sleep proxy: %d %s auth=%q", code, body, seenAuth)
	}
	if code, body := call("resume"); code != http.StatusNotImplemented || !strings.Contains(body, "unsupported") {
		t.Fatalf("unsupported must pass through as 501: %d %s", code, body)
	}
	if code, _ := call("sleep"); code == 200 {
		// unknown node → 404
		rr := chi.NewRouter()
		rr.Post("/nodes/{id}/sleep", srv.sleepWorker)
		rec := httptest.NewRecorder()
		rr.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/nodes/nope/sleep", nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("unknown node: %d", rec.Code)
		}
	}
}
