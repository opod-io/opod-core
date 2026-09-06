package agent

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestSupervisor_RestartOnCrash verifies that a process flagged with Restart:true
// is automatically re-launched after an abnormal exit, with the Restarts counter
// incremented, and that an explicit Stop() suppresses further restarts.
func TestSupervisor_RestartOnCrash(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("script-based test relies on POSIX shell")
	}

	dir := t.TempDir()
	counter := filepath.Join(dir, "count")
	script := filepath.Join(dir, "flaky.sh")

	// Script: increment a counter file, sleep 50ms, then exit 1. Each restart
	// bumps the file so the test can assert how many times the supervisor
	// re-launched us.
	if err := os.WriteFile(script, []byte(`#!/bin/sh
n=$(cat `+counter+` 2>/dev/null || echo 0)
echo $((n+1)) > `+counter+`
sleep 0.05
exit 1
`), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	sup := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer sup.StopAll()

	_, err := sup.Start(context.Background(), ProcessSpec{
		ID:             "flaky",
		Command:        script,
		Restart:        true,
		MaxRestarts:    3,
		RestartBackoff: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for the supervisor to exhaust its retries (3 restarts + 1 initial
	// run = 4 launches total). Generous timeout to absorb backoff jitter.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		info, ok := sup.Get("flaky")
		if !ok {
			t.Fatal("process disappeared from supervisor")
		}
		if info.Status == "crashloop" {
			if info.Restarts != 3 {
				t.Errorf("Restarts = %d, want 3 (MaxRestarts)", info.Restarts)
			}
			// Verify the counter file shows the launches actually happened.
			data, _ := os.ReadFile(counter)
			n := string(data)
			// 1 initial + 3 restarts = "4"
			if got := trimSpace(n); got != "4" {
				t.Errorf("counter = %q, want 4 (1 initial + 3 restarts)", got)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	info, _ := sup.Get("flaky")
	t.Fatalf("never reached crashloop within deadline; current status=%q restarts=%d", info.Status, info.Restarts)
}

// TestSupervisor_StopSuppressesRestart asserts that an explicit Stop() prevents
// any subsequent restart, even if the process would normally exit with an error.
func TestSupervisor_StopSuppressesRestart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("script-based test relies on POSIX shell")
	}

	dir := t.TempDir()
	script := filepath.Join(dir, "loop.sh")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
while true; do sleep 10; done
`), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	sup := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))

	if _, err := sup.Start(context.Background(), ProcessSpec{
		ID:             "looper",
		Command:        script,
		Restart:        true,
		MaxRestarts:    5,
		RestartBackoff: 10 * time.Millisecond,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Let it run for a moment so it's clearly in "running" before we Stop.
	time.Sleep(100 * time.Millisecond)

	if err := sup.Stop("looper"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// After Stop, the process should be removed from the supervisor's map
	// and there must be no automatic restart.
	if _, ok := sup.Get("looper"); ok {
		t.Error("process still in supervisor after Stop")
	}
	// Sleep past one backoff window — if a restart happened despite Stop,
	// it'd re-appear in the map.
	time.Sleep(150 * time.Millisecond)
	if _, ok := sup.Get("looper"); ok {
		t.Error("supervisor restarted a Stopped process")
	}
}

// trimSpace is a tiny helper to avoid pulling in strings just for TrimSpace.
func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\n' || s[0] == '\t' || s[0] == '\r') {
		s = s[1:]
	}
	for len(s) > 0 {
		c := s[len(s)-1]
		if c != ' ' && c != '\n' && c != '\t' && c != '\r' {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}
