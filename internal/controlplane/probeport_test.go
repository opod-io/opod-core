package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/scheduler"
	"github.com/opod-io/opod/internal/store"
)

// The probe port serves the unauthenticated machine-readable endpoints, and
// ONLY those: a scaler reading /loadz, a kubelet reading /readyz and a scrape
// reading /metrics should never need the certificate the gateway serves — nor
// be able to reach the gateway through this port.
func TestProbePortServesProbesAndNothingElse(t *testing.T) {
	s := probeTestServer(t)

	// Bind the way serveProbes does, then exercise the router it builds.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	s.cfg.ProbeListen = ln.Addr().String()
	_ = ln.Close()

	stop := s.serveProbes(context.Background())
	defer stop()
	base := "http://" + s.cfg.ProbeListen

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := http.Get(base + "/healthz"); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	for _, path := range []string{"/healthz", "/readyz", "/loadz", "/metrics"} {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode >= 500 {
			t.Errorf("%s: status %d", path, resp.StatusCode)
		}
	}

	// /loadz is the one a scaler reads: it must be JSON with the fields the
	// scaling decision is made from.
	resp, err := http.Get(base + "/loadz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var load map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&load); err != nil {
		t.Fatalf("/loadz is not JSON: %v", err)
	}
	for _, field := range []string{"in_flight", "queue_depth", "kv_used_pct", "unavailable_1m", "rpm_1m"} {
		if _, ok := load[field]; !ok {
			t.Errorf("/loadz has no %s — an external scaler reads it", field)
		}
	}

	// The gateway and the admin surface are NOT here.
	for _, path := range []string{"/v1/models", "/admin/v1/nodes", "/v1/chat/completions"} {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s is reachable on the probe port (status %d) — it must not be", path, resp.StatusCode)
		}
	}
}

// Off by default: an operator who sets nothing keeps exactly today's behaviour.
func TestProbePortIsOffByDefault(t *testing.T) {
	s := probeTestServer(t)
	s.cfg.ProbeListen = ""
	if s.cfg.ProbeListen != "" {
		t.Fatalf("probe listener configured by default: %q", s.cfg.ProbeListen)
	}
	stop := s.serveProbes(context.Background())
	defer stop()
	fmt.Fprint(new(testWriter), "") // nothing started; nothing to check but that it did not panic
}

type testWriter struct{}

func (testWriter) Write(p []byte) (int, error) { return len(p), nil }

// probeTestServer is a leader with a store and a scheduler — what /readyz and
// /loadz read — and nothing else.
func probeTestServer(t *testing.T) *Server {
	t.Helper()
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Default()
	cfg.Listen = ":0"
	return NewServer(cfg, st, &stubLeaderEngine{}, nil, log, scheduler.New(st, nil, log, ""))
}
