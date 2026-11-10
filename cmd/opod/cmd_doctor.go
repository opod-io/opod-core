package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/opod-io/opod-core/internal/agent"
	"github.com/opod-io/opod-core/internal/config"
	"github.com/opod-io/opod-core/internal/models"
)

// doctorCheck is one row of the report. A problem is something to fix; a
// note is worth knowing and needs nothing.
type doctorCheck struct {
	Name   string `json:"name"`
	State  string `json:"state"` // ok | problem | note
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

func cmdDoctor(args []string) {
	if wantsHelp(args) {
		showHelp(helpSpec{
			name:    "doctor",
			summary: "check this host: config, leader, admin key, port, engine, data dir, catalog",
			usage:   "opod doctor [--json]",
			examples: []string{
				"opod doctor              # every check, each problem with its fix; exit 1 when any is found",
				"opod doctor --json       # the same, for a script",
			},
		})
	}
	rest, asJSON := extractJSONFlag(args)
	if len(rest) > 0 {
		dieUsageOf("doctor", "unknown argument %q (doctor takes only --json)", rest[0])
	}
	checks := runDoctor()
	problems := 0
	for _, c := range checks {
		if c.State == "problem" {
			problems++
		}
	}
	if asJSON {
		emitJSON(map[string]any{"checks": checks, "problems": problems})
	} else {
		fmt.Println("Opod doctor")
		fmt.Println("============")
		for _, c := range checks {
			mark := ansiFor(os.Stdout, "1;32", "✔")
			switch c.State {
			case "problem":
				mark = ansiFor(os.Stdout, "1;31", "✖")
			case "note":
				mark = ansiFor(os.Stdout, "1;34", "▶")
			}
			fmt.Printf("%s %s: %s\n", mark, c.Name, c.Detail)
			if c.Fix != "" {
				fmt.Printf("    → %s\n", c.Fix)
			}
		}
		fmt.Println()
		if problems == 0 {
			fmt.Println("all checks passed")
		} else {
			fmt.Printf("%d problem(s) found — each says its fix above\n", problems)
		}
	}
	if problems > 0 {
		os.Exit(1)
	}
}

// runDoctor runs every check, whatever an earlier one found: a bad config or
// an unknown engine is a row of the report, never the end of it.
func runDoctor() []doctorCheck {
	var out []doctorCheck
	add := func(name, state, detail, fix string) {
		out = append(out, doctorCheck{Name: name, State: state, Detail: detail, Fix: fix})
	}
	caps := agent.Detect()
	add("hardware", "ok", fmt.Sprintf("%s/%s · %d cores · %d GB RAM", caps.OS, caps.Arch, caps.CPUCores, caps.RAMGB), "")
	for _, g := range caps.GPUs {
		add("GPU", "ok", fmt.Sprintf("%s (%d GB)", g.Name, g.VRAMGB), "")
	}

	path := config.DefaultPath()
	cfg, err := config.Load("")
	switch {
	case err != nil:
		add("config", "problem", err.Error(), "fix or remove "+path+" — the checks below use the defaults")
		cfg = config.Default()
	case fileExists(path):
		add("config", "ok", path, "")
	default:
		add("config", "note", "no config file at "+path+" — the defaults are used", "opod up writes one on its first run; edit it with opod config edit")
	}

	addr := cfg.Listen
	if addr == "" {
		addr = ":8080"
	}
	leaderUp := leaderAnswers(cfg)
	if leaderUp {
		add("leader", "ok", "answering at "+localLeaderURL(cfg), "")
	} else {
		add("leader", "note", "no leader answering at "+localLeaderURL(cfg), "start one with opod up — on a worker host this is expected")
	}
	switch {
	case leaderUp:
		who := "this host's opod leader"
		if pid, perr := readPID(cfg); perr == nil {
			who += fmt.Sprintf(" (pid %d)", pid)
		}
		add("listen port", "ok", addr+" is "+who, "")
	case portAvailable(addr):
		add("listen port", "ok", addr+" is free", "")
	default:
		add("listen port", "problem", addr+" is in use by another program", "stop it, or set listen: in "+path+" or OPOD_LISTEN")
	}

	keyPath := localAdminKeyPath(cfg)
	switch {
	case readLocalAdminKey(cfg) != "":
		add("admin key", "ok", keyPath, "")
	case leaderUp:
		add("admin key", "problem", "a leader answers but there is no admin key at "+keyPath, "run admin commands on the host where opod up wrote it, or set OPOD_TOKEN to an admin key")
	default:
		add("admin key", "note", "none at "+keyPath, "opod up writes it on its first run")
	}

	eng, err := engineFromConfig(cfg)
	if err != nil {
		add("engine", "problem", err.Error(), "set engine.preferred in "+path+" (or OPOD_ENGINE) to one of the valid names")
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		herr := eng.Health(ctx)
		cancel()
		if herr != nil {
			add("engine", "problem", herr.Error(), engineStartHint(eng.Name()))
		} else {
			add("engine", "ok", fmt.Sprintf("%s healthy at %s", eng.Name(), eng.Endpoint()), "")
		}
		// MLX-LM's own server defaults to the leader's port, 8080.
		if _, eport, _ := net.SplitHostPort(strings.TrimPrefix(strings.TrimPrefix(eng.Endpoint(), "http://"), "https://")); eport != "" {
			if _, lport, _ := net.SplitHostPort(addr); lport == eport {
				add("engine port", "problem", fmt.Sprintf("the %s endpoint %s uses the leader's port %s", eng.Name(), eng.Endpoint(), lport),
					"start the engine on another port and set its endpoint (engine."+eng.Name()+"_endpoint in "+path+"), e.g. mlx_lm.server --port 8090")
			}
		}
		if eng.Name() == "ollama" {
			if p, lerr := exec.LookPath("ollama"); lerr == nil {
				add("ollama binary", "ok", p, "")
			} else {
				add("ollama binary", "note", "not in PATH", "install: "+ollamaInstallHint())
			}
		}
	}

	// llama.cpp is only needed for sharded models and engine.preferred=llamacpp.
	_, rpcErr := exec.LookPath("rpc-server")
	srvPath, srvErr := exec.LookPath("llama-server")
	switch {
	case rpcErr == nil && srvErr == nil:
		add("llama.cpp", "ok", "rpc-server and llama-server on PATH", "")
	case srvErr == nil:
		add("llama.cpp", "note", "llama-server at "+srvPath+", no rpc-server — needed only for sharded models", "build it with cmake -DGGML_RPC=ON: https://github.com/ggml-org/llama.cpp")
	default:
		add("llama.cpp", "note", "not installed — needed only for sharded models or engine.preferred=llamacpp", "install: "+llamaInstallHint())
	}

	if _, err := os.Stat(cfg.DataDir); err == nil {
		add("data dir", "ok", cfg.DataDir, "")
	} else {
		add("data dir", "note", "missing: "+cfg.DataDir, "opod up creates it, or: mkdir -p "+cfg.DataDir)
	}

	if entries, err := loadCatalog(cfg); err == nil {
		add("catalog", "ok", fmt.Sprintf("%d entries%s", len(entries), catalogPolicyNote(cfg.Env)), "")
	} else if errors.Is(err, models.ErrCatalogSignature) {
		add("catalog", "problem", err.Error(), "sign the file (minisign -S -m <file>), remove it, or review OPOD_CATALOG_PUBKEY / OPOD_CATALOG_REQUIRE_SIGNED")
	} else {
		add("catalog", "problem", err.Error(), "the built-in catalog is in this binary: check the files you added in OPOD_CATALOG_DIR (default ~/.opod/catalog), or unset it")
	}
	return out
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// ollamaInstallHint and llamaInstallHint name the install for this OS.
func ollamaInstallHint() string {
	if runtime.GOOS == "darwin" {
		return "brew install ollama"
	}
	return "curl -fsSL https://ollama.com/install.sh | sh"
}

func llamaInstallHint() string {
	if runtime.GOOS == "darwin" {
		return "brew install llama.cpp"
	}
	return "a release build from https://github.com/ggml-org/llama.cpp/releases, or your distribution's llama.cpp package"
}

func portAvailable(addr string) bool {
	// A bare port ("8080") gets its colon; a host:port is used as it is
	// (T10.21: "127.0.0.1:8080" became ":127.0.0.1:8080" and read as in use).
	if !strings.Contains(addr, ":") {
		addr = ":" + addr
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// catalogPolicyNote says, beside the entry count, which signature policy the
// directory files were loaded under — nothing when none is configured.
func catalogPolicyNote(env config.Env) string {
	switch {
	case env.CatalogPubKey == "":
		return ""
	case env.CatalogMustSign:
		return " (directory files: minisign signature required)"
	default:
		return " (directory files: minisign signature verified when present)"
	}
}
