package leader

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/agent"
	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/store"
)

// freeAddr is a loopback address nothing listens on at the moment it returns.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// The plain listener serves the WHOLE API — the main listener's own handler —
// in clear text, while the main listener keeps its certificate. A gateway that
// cannot originate TLS to a leader's self-signed certificate reaches /v1 here;
// everyone else still gets TLS on the main port.
func TestPlainListenerServesTheWholeAPIBesideTLS(t *testing.T) {
	dir := t.TempDir()
	crt, key := selfSigned(t, dir)
	cfg := config.Default()
	cfg.Listen = "127.0.0.1:0"
	cfg.TLSCert, cfg.TLSKey = crt, key
	cfg.DataDir = dir
	cfg.PlainListen = freeAddr(t)
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := NewServer(cfg, st, &stubLeaderEngine{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Start(ctx) }()

	base := "http://" + cfg.PlainListen
	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := client.Get(base + "/healthz")
		if err == nil {
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the plain listener never answered: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The request API and the admin surface are routed here, unlike on the
	// probe port: an answer that is not 404 is the main router answering
	// (the admin route asks for its key, which is the point — same handler,
	// same checks).
	for _, path := range []string{"/healthz", "/readyz", "/v1/models", "/admin/v1/capabilities"} {
		resp, err := client.Get(base + path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			t.Errorf("%s on the plain listener: status %d — it must be the main router", path, resp.StatusCode)
		}
	}
	resp, err := client.Get(base + "/admin/v1/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("/admin/v1 without a key on the plain listener: status %d, want 401 — the same checks as the main port", resp.StatusCode)
	}

	// The main listener still speaks TLS only.
	for srv.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	trusting, err := agent.NewLeaderClient(crt, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := trusting.Get("https://" + srv.Addr() + "/healthz"); err != nil {
		t.Fatalf("the main listener over TLS: %v", err)
	} else {
		resp.Body.Close()
	}
	if resp, err := client.Get("http://" + srv.Addr() + "/healthz"); err == nil && resp.StatusCode == http.StatusOK {
		resp.Body.Close()
		t.Fatal("the main TLS listener answered plain http with 200")
	}
}

// Off by default: an operator who sets nothing gets no second listener.
func TestPlainListenerIsOffByDefault(t *testing.T) {
	if got := config.Default().PlainListen; got != "" {
		t.Fatalf("plain listener configured by default: %q", got)
	}
	s := probeTestServer(t)
	stop, err := s.servePlain(http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	stop()
}

// An address that cannot be bound is an error, not a log line: whoever set it
// routes requests there, and a leader Ready without it would refuse them all.
func TestPlainListenerThatCannotBindFailsStart(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	s := probeTestServer(t)
	s.cfg.Listen = "127.0.0.1:0"
	s.cfg.PlainListen = taken.Addr().String()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Start(ctx); err == nil || !strings.Contains(err.Error(), "plain listener") {
		t.Fatalf("Start with a plain listener it could not bind: %v — it must refuse, naming the listener", err)
	}
}
