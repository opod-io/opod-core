//go:build unix

package agent

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// When a process's context ends — a failed health probe, a stop — the kill
// reaches the process's whole group: an engine's own children (vLLM's Ray
// workers) must not outlive it holding the card (T22.4). The default kill
// reached only the direct child.
func TestAProcessContextEndKillsItsChildrenToo(t *testing.T) {
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "child.pid")
	sup := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer sup.StopAll()
	if _, err := sup.Start(context.Background(), ProcessSpec{ID: "engine", Command: "/bin/sh",
		Args: []string{"-c", "sleep 300 & echo $! > " + pidfile + "; wait"}}); err != nil {
		t.Fatal(err)
	}
	var child int
	for i := 0; i < 200 && child == 0; i++ {
		if b, err := os.ReadFile(pidfile); err == nil {
			child, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if child == 0 {
		t.Fatal("the engine never forked its child")
	}
	sup.mu.RLock()
	p := sup.procs["engine"]
	sup.mu.RUnlock()
	p.mu.Lock()
	cancel := p.cancel
	p.mu.Unlock()
	cancel() // what a failed health probe does (markFailed)
	for i := 0; i < 300; i++ {
		if syscall.Kill(child, 0) != nil {
			return // gone
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the engine's child %d outlived the engine", child)
}
