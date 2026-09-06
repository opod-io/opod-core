package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/opod-io/opod/internal/agent"
	"github.com/opod-io/opod/internal/models"
)

func cmdDoctor(args []string) {
	if wantsHelp(args) {
		showHelp(helpSpec{
			name:    "doctor",
			summary: "diagnose common setup problems (engine, port, catalog, hardware)",
			usage:   "opod doctor",
			examples: []string{
				"opod doctor              # run all checks, print fix commands for failures",
			},
		})
	}
	cfg := loadConfigOrExit()
	fmt.Println("Opod doctor")
	fmt.Println("============")

	// Hardware
	caps := agent.Detect()
	ok(os.Stdout, "hardware: %s/%s · %d cores · %d GB RAM",
		caps.OS, caps.Arch, caps.CPUCores, caps.RAMGB)
	for _, g := range caps.GPUs {
		ok(os.Stdout, "GPU: %s (%d GB)", g.Name, g.VRAMGB)
	}

	// Listen port
	addr := cfg.Listen
	if addr == "" {
		addr = ":8080"
	}
	if portAvailable(addr) {
		ok(os.Stdout, "listen port %s available", addr)
	} else {
		warn(os.Stdout, "listen port %s already in use", addr)
	}

	// Ollama
	if path, err := exec.LookPath("ollama"); err == nil {
		ok(os.Stdout, "ollama binary at %s", path)
	} else {
		warn(os.Stdout, "ollama not found in PATH — install: brew install ollama")
	}

	// llama.cpp — only required for sharded models (rpc-server) and the
	// llamacpp engine's auto-spawn (llama-server). For a default single-node
	// Ollama setup, missing binaries are not an error.
	rpcPath, rpcErr := exec.LookPath("rpc-server")
	srvPath, srvErr := exec.LookPath("llama-server")
	switch {
	case rpcErr == nil && srvErr == nil:
		ok(os.Stdout, "llama.cpp binaries present — rpc-server at %s, llama-server at %s", rpcPath, srvPath)
	case rpcErr != nil && srvErr != nil:
		note(os.Stdout, "llama.cpp not installed — needed only for sharded models / engine.preferred=llamacpp")
		note(os.Stdout, "  → install: brew install llama.cpp  (macOS) · apt: see https://github.com/ggml-org/llama.cpp")
	case rpcErr != nil && srvErr == nil:
		// Common on macOS: Homebrew's llama.cpp bottle ships llama-server but
		// not rpc-server. rpc-server requires a source build with -DGGML_RPC=ON.
		note(os.Stdout, "llama-server found at %s; rpc-server missing", srvPath)
		note(os.Stdout, "  → rpc-server is not in the Homebrew bottle; only needed for sharded models")
		note(os.Stdout, "  → if you need sharding, build from source: https://github.com/ggml-org/llama.cpp (cmake -DGGML_RPC=ON)")
	default:
		warn(os.Stdout, "rpc-server found at %s but llama-server missing", rpcPath)
		warn(os.Stdout, "  → install llama.cpp: brew install llama.cpp  (macOS)")
	}

	// Configured engine daemon
	eng := newEngineFromConfig(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := eng.Health(ctx); err != nil {
		warn(os.Stdout, "%s engine not reachable: %v", eng.Name(), err)
		warn(os.Stdout, "  → %s", engineStartHint(eng.Name()))
	} else {
		ok(os.Stdout, "%s engine healthy at %s", eng.Name(), eng.Endpoint())
	}

	// Data dir
	if _, err := os.Stat(cfg.DataDir); err == nil {
		ok(os.Stdout, "data dir: %s", cfg.DataDir)
	} else {
		warn(os.Stdout, "data dir missing: %s", cfg.DataDir)
	}

	// Catalog — `opod up` hard-fails without it, so escalate to a warning
	// with an actionable hint pointing at the user-writable install location.
	if entries, err := models.LoadCatalog(cfg.CatalogDir); err == nil {
		ok(os.Stdout, "catalog: %d entries", len(entries))
	} else {
		warn(os.Stdout, "catalog: %v", err)
		warn(os.Stdout, "  → reinstall to drop the bundled catalog at ~/.opod/catalog,")
		warn(os.Stdout, "    or set OPOD_CATALOG_DIR to a directory of *.yaml entries")
	}

	fmt.Println()
}

func portAvailable(addr string) bool {
	if !strings.HasPrefix(addr, ":") {
		addr = ":" + addr
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}
