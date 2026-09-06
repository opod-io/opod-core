// Single-node llama-server auto-launch. Lets `opod up` spawn a local
// `llama-server` process via the same Supervisor that already runs the
// sharding coordinator — so a user with engine.preferred=llamacpp doesn't
// have to start the engine binary manually before `opod up`.
//
// This is the non-RPC counterpart to the coordinator launch in
// sharding.go: identical ProcessSpec shape, just without `--rpc`.
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"time"

	"github.com/opod-io/opod/internal/agent"
	"github.com/opod-io/opod/internal/models"
)

// LlamaCppLaunchSpec describes how to launch a single-node llama-server.
type LlamaCppLaunchSpec struct {
	// Entry is the catalog entry to serve. Source.Repo is preferred (uses
	// llama-server's -hf flag, downloads via the HF cache); Source.Path is
	// used as a fallback for already-downloaded GGUF files. If both are
	// empty EnsureLlamaServer returns an error rather than guessing.
	Entry *models.Entry
	// Port is where llama-server will listen (matches engine.llamacpp_endpoint).
	Port int
	// CtxSize, if >0, is passed as --ctx-size. Defaults to llama-server's own.
	CtxSize int
	// ExtraArgs are appended verbatim — e.g. ["-ngl", "999"] for full GPU offload.
	ExtraArgs []string
}

// LlamaCppProcessID returns the supervisor process id used for an entry.
// Exported so callers can Stop or look up the process later if needed.
func LlamaCppProcessID(entryID string) string {
	return "llamacpp-" + safeID(entryID)
}

// StartHealthWatchdog launches a goroutine that periodically probes the
// engine and force-restarts the spawned llama-server process on
// consecutive failures. The Supervisor already handles "process crashed"
// via Restart=true; this covers the harder case where the process is
// still running but unresponsive (hung GGUF load, deadlocked endpoint,
// etc.). Stops when ctx is cancelled.
//
// Tunables are conservative on purpose: a momentary blip during model
// load shouldn't trigger a restart, and the restart itself should not
// itself become a flap source.
func StartHealthWatchdog(ctx context.Context, sup *agent.Supervisor, log *slog.Logger, eng engineHealthchecker, spec LlamaCppLaunchSpec) {
	go func() {
		const (
			probeInterval     = 30 * time.Second
			probeTimeout      = 5 * time.Second
			failuresToRestart = 3
		)
		consecutive := 0
		ticker := time.NewTicker(probeInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			hctx, cancel := context.WithTimeout(ctx, probeTimeout)
			err := eng.Health(hctx)
			cancel()
			if err == nil {
				if consecutive > 0 {
					log.Info("engine recovered", "engine", eng.Name())
				}
				consecutive = 0
				continue
			}
			consecutive++
			log.Warn("engine health failed",
				"engine", eng.Name(),
				"consecutive_failures", consecutive,
				"err", err.Error())
			if consecutive < failuresToRestart {
				continue
			}
			procID := LlamaCppProcessID(spec.Entry.ID)
			log.Warn("restarting unresponsive engine via watchdog",
				"engine", eng.Name(),
				"proc_id", procID)
			_ = sup.Stop(procID)
			// Give the supervisor a moment to clean up before we re-spawn.
			time.Sleep(2 * time.Second)
			if _, rerr := EnsureLlamaServer(ctx, sup, log, spec); rerr != nil {
				log.Error("watchdog restart failed", "err", rerr.Error())
				// Don't reset the counter — next tick will try again.
				continue
			}
			log.Info("engine restarted by watchdog", "engine", eng.Name())
			consecutive = 0
		}
	}()
}

// engineHealthchecker is the minimal surface the watchdog needs from the
// engines package. Defined here so the scheduler doesn't pull in the
// full engines.Engine interface for what is essentially Health() + Name().
type engineHealthchecker interface {
	Health(context.Context) error
	Name() string
}

// EnsureLlamaServer launches a llama-server for spec.Entry via sup and
// waits for the port to accept TCP. Idempotent: if the supervisor already
// has a process under the same id, returns its existing ProcessInfo.
// The supervisor's StopAll (deferred in cmd_up) tears the process down on
// opod shutdown.
func EnsureLlamaServer(ctx context.Context, sup *agent.Supervisor, log *slog.Logger, spec LlamaCppLaunchSpec) (*agent.ProcessInfo, error) {
	if spec.Entry == nil {
		return nil, fmt.Errorf("llamacpp auto-spawn: nil catalog entry")
	}
	if spec.Port <= 0 {
		return nil, fmt.Errorf("llamacpp auto-spawn: port must be > 0")
	}
	if _, err := exec.LookPath("llama-server"); err != nil {
		return nil, fmt.Errorf("llama-server not found in PATH (install: brew install llama.cpp)")
	}

	procID := LlamaCppProcessID(spec.Entry.ID)
	if existing, ok := sup.Get(procID); ok && existing.Status == "running" {
		return existing, nil
	}

	args := []string{}
	switch {
	case spec.Entry.Source.Repo != "":
		args = append(args, "-hf", spec.Entry.Source.Repo)
	case spec.Entry.Source.Path != "":
		args = append(args, "-m", spec.Entry.Source.Path)
	default:
		return nil, fmt.Errorf("catalog entry %s has no source.repo (HF GGUF) or source.path (local GGUF) for llama-server", spec.Entry.ID)
	}
	args = append(args,
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(spec.Port),
	)
	if spec.CtxSize > 0 {
		args = append(args, "--ctx-size", strconv.Itoa(spec.CtxSize))
	}
	args = append(args, spec.ExtraArgs...)

	procSpec := agent.ProcessSpec{
		ID:             procID,
		Command:        "llama-server",
		Args:           args,
		HealthPort:     spec.Port,
		HealthHost:     "127.0.0.1",
		Restart:        true,
		MaxRestarts:    5,
		RestartBackoff: time.Second,
	}
	log.Info("auto-spawning llama-server", "model", spec.Entry.ID, "port", spec.Port)
	return sup.Start(ctx, procSpec)
}
