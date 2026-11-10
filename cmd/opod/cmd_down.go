package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/opod-io/opod-core/internal/engines"
	"github.com/opod-io/opod-core/internal/lifecycle"
)

func cmdDown(args []string) {
	fs := flag.NewFlagSet("down", flag.ExitOnError)
	noUnload := fs.Bool("no-unload", false, "leave models resident in the engine's memory (skip the default unload)")
	help := helpSpec{
		name:    "down",
		summary: "stop this host's leader (opod up) and release engine memory",
		usage:   "opod down [--no-unload]",
		flags:   fs,
		examples: []string{
			"opod down              # stop + unload models from engine RAM",
			"opod down --no-unload  # stop only; models stay resident (Ollama TTL applies)",
		},
		notes: []string{
			"`down` stops the leader `opod up` started on this host. A worker (`opod join`) is stopped with Ctrl-C or by stopping its container.",
			"`down` is a deliberate teardown, so it unloads resident models by default.",
			"Ctrl-C of `opod up` does NOT unload (fast dev restarts) — use --unload-on-exit there.",
			"Engines without an unload protocol (vLLM, MLX-LM) are skipped with a note; Opod-spawned llama-server processes are killed by `opod up`'s own shutdown.",
		},
	}
	// Bad flags: print usage to stderr and let ExitOnError exit 2.
	if wantsHelp(args) {
		showHelp(help)
	}
	parseFlags(fs, "down", args)

	cfg := loadConfigOrExit()
	pid, err := readPID(cfg)
	if err != nil {
		ok(os.Stdout, "opod is not running on this host (no PID file at %s) — nothing to stop", pidFilePath(cfg))
		return
	}
	if !processAlive(pid) {
		removePID(cfg)
		ok(os.Stdout, "pid %d is not running — removed the stale PID file %s; nothing to stop", pid, pidFilePath(cfg))
		return
	}
	// A PID file outlives its process, and the number can come back as
	// something else: signal only a process that is opod (or answers as this
	// host's leader). `down` once stopped a `sleep` that inherited the pid.
	if name := processName(pid); !strings.Contains(name, "opod") && !leaderAnswers(cfg) {
		removePID(cfg)
		warn("pid %d in %s is not opod (it is %q) — removed the stale PID file; nothing stopped", pid, pidFilePath(cfg), name)
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		die("stop pid %d: %v", pid, err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		die("stop pid %d: %v", pid, err)
	}
	note(os.Stdout, "stopping opod (pid %d)…", pid)
	// Wait for it to exit, so the unload below does not race its shutdown.
	deadline := time.Now().Add(10 * time.Second)
	for processAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	if processAlive(pid) {
		warn("pid %d is still running after 10s — check `opod status`; force it with `kill -9 %d`", pid, pid)
		os.Exit(1)
	}
	ok(os.Stdout, "opod (pid %d) stopped", pid)

	if *noUnload {
		return
	}
	// Deliberate teardown defaults to releasing engine memory. The engine
	// (e.g. the Ollama daemon) outlives the opod process, so this must
	// happen client-side after the server is down.
	eng := newEngineFromConfig(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := eng.Health(ctx); err != nil {
		// No engine running → nothing resident; stay quiet.
		return
	}
	mgr := &lifecycle.Manager{Engine: eng, Log: slog.Default()}
	n, err := mgr.UnloadAll(ctx)
	switch {
	case errors.Is(err, engines.ErrUnloadNotSupported):
		note(os.Stdout, "%s has no unload protocol — restart it to free RAM (Opod-spawned engines are already stopped)", eng.Name())
	case err != nil:
		warn("unload: %v (unloaded %d)", err, n)
	case n > 0:
		ok(os.Stdout, "unloaded %d model(s) from %s", n, eng.Name())
	}
}
