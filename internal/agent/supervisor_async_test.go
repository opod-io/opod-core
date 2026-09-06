package agent

import (
	"io"
	"log/slog"
	"net"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// freePort grabs a port the test can hand to a fake "engine".
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// TestStartAsync_SlowLoaderDoesNotFailTheStart is the regression for the bug that
// broke the remote shard coordinator (D4/D2).
//
// llama-server binds its port only AFTER loading the model, so readiness can take
// minutes. The worker used to run the readiness probe INSIDE the start request,
// which meant the leader's 60s HTTP client gave up and reported a failed create
// for a process that was loading perfectly well.
//
// StartAsync must return immediately with "starting" and let the probe finish on
// its own clock — well past any plausible client deadline.
func TestStartAsync_SlowLoaderDoesNotFailTheStart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("script-based test relies on POSIX shell")
	}
	if _, err := osexec.LookPath("python3"); err != nil {
		t.Skip("needs python3 to stand in for a slow-listening engine")
	}
	port := freePort(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "slowload.sh")

	// A process that "loads" for a while and only THEN starts listening — the
	// llama-server shape.
	if err := os.WriteFile(script, []byte(`#!/bin/sh
sleep 1
exec python3 -m http.server `+strconv.Itoa(port)+` --bind 127.0.0.1
`), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	sup := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer sup.StopAll()

	start := time.Now()
	info, err := sup.StartAsync(ProcessSpec{
		ID:           "slow",
		Command:      script,
		HealthPort:   port,
		HealthHost:   "127.0.0.1",
		ReadyTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("StartAsync: %v", err)
	}
	// The whole point: it returns without waiting for readiness.
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Fatalf("StartAsync blocked for %s — it must not wait for the readiness probe", took)
	}
	if info.Status != "starting" {
		t.Fatalf("status = %q, want %q", info.Status, "starting")
	}

	// And it does become ready, on its own clock, after the client would have
	// long since given up.
	deadline := time.Now().Add(20 * time.Second)
	for {
		got, ok := sup.Get("slow")
		if !ok {
			t.Fatal("process record vanished — a poller could not learn its fate")
		}
		if got.Status == "running" {
			return // success
		}
		if got.Status == "failed" || got.Status == "crashloop" {
			t.Fatalf("process %s: %s", got.Status, got.ExitErr)
		}
		if time.Now().After(deadline) {
			t.Fatalf("never became ready (stuck in %q)", got.Status)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestStartAsync_FailureIsPollable: when a process dies during startup, the
// record must SURVIVE as "failed" with the engine's own output attached.
// Start() deletes the record on failure, which would leave a poller staring at
// "process not found" — the exact unactionable-502 problem we set out to kill.
func TestStartAsync_FailureIsPollable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("script-based test relies on POSIX shell")
	}
	port := freePort(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "boom.sh")

	// Dies immediately, with a complaint on stderr — the llama.cpp abort shape.
	if err := os.WriteFile(script, []byte(`#!/bin/sh
echo "GGML_ASSERT: rpc backend unreachable" >&2
exit 1
`), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	sup := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer sup.StopAll()

	if _, err := sup.StartAsync(ProcessSpec{
		ID:           "boom",
		Command:      script,
		HealthPort:   port,
		HealthHost:   "127.0.0.1",
		ReadyTimeout: 3 * time.Second,
	}); err != nil {
		t.Fatalf("StartAsync: %v", err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for {
		got, ok := sup.Get("boom")
		if !ok {
			t.Fatal("failed process was deleted — a poller cannot learn WHY it failed")
		}
		if got.Status == "failed" {
			// The engine's own words must ride along on the error.
			if !strings.Contains(got.ExitErr, "GGML_ASSERT") {
				t.Fatalf("ExitErr does not carry the engine's output: %q", got.ExitErr)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("never reached failed (stuck in %q)", got.Status)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
