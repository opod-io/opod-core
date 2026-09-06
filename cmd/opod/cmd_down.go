package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"syscall"
	"time"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/lifecycle"
)

func cmdDown(args []string) {
	fs := flag.NewFlagSet("down", flag.ExitOnError)
	noUnload := fs.Bool("no-unload", false, "leave models resident in the engine's memory (skip the default unload)")
	help := helpSpec{
		name:    "down",
		summary: "stop the local opod node and release engine memory",
		usage:   "opod down [--no-unload]",
		flags:   fs,
		examples: []string{
			"opod down              # stop + unload models from engine RAM",
			"opod down --no-unload  # stop only; models stay resident (Ollama TTL applies)",
		},
		notes: []string{
			"`down` is a deliberate teardown, so it unloads resident models by default.",
			"Ctrl-C of `opod up` does NOT unload (fast dev restarts) — use --unload-on-exit there.",
			"Engines without an unload protocol (vLLM, MLX-LM) are skipped with a note; Opod-spawned llama-server processes are killed by `opod up`'s own shutdown.",
		},
	}
	// Bad flags: print usage to stderr and let ExitOnError exit 2.
	fs.Usage = func() { showUsageErr(help) }
	if wantsHelp(args) {
		showHelp(help)
	}
	_ = fs.Parse(args)

	cfg := loadConfigOrExit()
	pid, err := readPID(cfg)
	if err != nil {
		die("no PID file at %s (is opod running?)", pidFilePath(cfg))
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		die("find process %d: %v", pid, err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		// Process already gone (crashed, killed, rebooted) — the PID file
		// is stale. Clean it up so the next `opod down` / `opod up`
		// doesn't trip over it, and say so instead of a raw signal error.
		if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
			removePID(cfg)
			die("process %d is not running — removed stale PID file %s", pid, pidFilePath(cfg))
		}
		die("signal pid %d: %v", pid, err)
	}
	ok(os.Stdout, "sent SIGTERM to pid %d", pid)

	// Wait for the server to actually exit (signal 0 probes liveness) so
	// the unload below doesn't race its graceful shutdown.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			break // process gone
		}
		time.Sleep(200 * time.Millisecond)
	}

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
		warn(os.Stdout, "unload: %v (unloaded %d)", err, n)
	case n > 0:
		ok(os.Stdout, "unloaded %d model(s) from %s", n, eng.Name())
	}
}
