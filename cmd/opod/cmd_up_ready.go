package main

// `opod up` — the ready banner, network posture and first-run wizard.

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"bufio"
	"net/url"

	"github.com/mattn/go-isatty"
	"github.com/opod-io/opod-core/internal/agent"
	"github.com/opod-io/opod-core/internal/config"
	"github.com/opod-io/opod-core/internal/engines"
	"github.com/opod-io/opod-core/internal/models"
	"github.com/opod-io/opod-core/internal/store"
)

func printReady(cfg *config.Config, adminKey string) {
	base := localLeaderURL(cfg)
	if cfg.ExternalURL != "" {
		base = cfg.ExternalURL
	}
	fmt.Println()
	fmt.Println("  Opod is ready.")
	fmt.Println()
	fmt.Printf("  API:        %s/v1\n", base)
	fmt.Printf("  Health:     %s/healthz\n", base)
	if adminKey != "" {
		// First run: a brand-new admin key was generated. Walk the operator
		// through every next step so they don't have to dig through README.
		fmt.Println()
		fmt.Println("  Admin API key (shown once — store it now):")
		emitSecret(adminKey)
		fmt.Println()
		fmt.Println("  Next steps:")
		// Column width tuned to the widest label ("Mint another admin key:"
		// at 23 chars) so the right-hand commands line up no matter which
		// label is longest.
		nx := func(label, val string) {
			fmt.Printf("    →  %-25s%s\n", label, val)
		}
		nx("Wire up Aider:", "opod connect aider")
		nx("Wire up Cursor:", "opod connect cursor")
		nx("See all clients:", "opod connect --list")
		nx("Mint another admin key:", "opod token create ops --admin")
		fmt.Println()
		fmt.Println("  Quick test from the shell:")
		fmt.Printf("    curl %s/v1/chat/completions \\\n", base)
		fmt.Printf("      -H 'Authorization: Bearer %s' \\\n", adminKey)
		fmt.Println(`      -H 'Content-Type: application/json' \`)
		fmt.Printf("      -d '{\"model\":\"%s\",\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}]}'\n", cfg.Router.DefaultModel)
	} else {
		printReturningAdminKey(os.Stdout, localAdminKeyPath(cfg), readLocalAdminKey(cfg) != "")
		fmt.Println("  Wire up a tool:")
		fmt.Println("    opod connect cursor        # or: aider, continue, zed, …")
		fmt.Println("    opod connect --list        # see all supported clients")
	}
	printNetworkPosture(os.Stdout, cfg)
	fmt.Println()
	fmt.Println("  Press Ctrl-C to stop.")
	fmt.Println()
}

// printNetworkPosture lists every outbound network call Opod can make
// from this process. Designed so an operator running `opod up` can audit
// the privacy posture at a glance — no surprises, no silent calls. Reads
// the live config so it reflects what *this* invocation will actually do.
func printNetworkPosture(w io.Writer, cfg *config.Config) {
	fmt.Fprintln(w)
	// Only what exists is named here. This line once listed ui / egress /
	// callbacks switches for surfaces the binary no longer has.
	if cfg.Surfaces.Managed {
		fmt.Fprintln(w, "  Mode:            "+cfg.Surfaces.Summary()+"  (OPOD_MANAGED=1: run by an external manager; the store is an in-memory cache unless OPOD_STORAGE_DSN is set)")
	} else {
		fmt.Fprintln(w, "  Mode:            "+cfg.Surfaces.Summary())
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  Network behavior on this node:")

	// Tracing
	if cfg.Observability.OTLPEndpoint == "" {
		fmt.Fprintln(w, "    · Tracing:       OFF  (set OPOD_OTLP_ENDPOINT=… to your collector to enable)")
	} else {
		fmt.Fprintf(w, "    · Tracing:       → %s  (your collector; set OFF by clearing OPOD_OTLP_ENDPOINT)\n", cfg.Observability.OTLPEndpoint)
	}

	// Log export — the same audit: where this process's records go besides stderr.
	if cfg.Env.OTLPLogsEndpoint == "" {
		fmt.Fprintln(w, "    · Log export:    OFF  (set OPOD_OTLP_LOGS_ENDPOINT=… to your collector to enable)")
	} else {
		fmt.Fprintf(w, "    · Log export:    → %s  (your collector, beside stderr; set OFF by clearing OPOD_OTLP_LOGS_ENDPOINT)\n", cfg.Env.OTLPLogsEndpoint)
	}

	// Usage push — the same audit for the per-request records (T32.34).
	if cfg.Env.UsageOTLPEndpoint == "" {
		fmt.Fprintln(w, "    · Usage push:    OFF  (records held in memory, oldest dropped at the cap; set OPOD_USAGE_OTLP_ENDPOINT=… to send them)")
	} else {
		fmt.Fprintf(w, "    · Usage push:    → %s  (one OTLP log record per request, freed on a 2xx)\n", cfg.Env.UsageOTLPEndpoint)
	}

	fmt.Fprintln(w, "    · Telemetry:     none. Opod never reports installs, usage, errors, or any data to opod.io.")
}

// isLlamaCppEngine matches every alias the engine registry accepts for
// the llama.cpp driver. Mirrors the cases in newEngineFromConfig +
// internal/engines/registry.go so the auto-spawn trigger stays in sync.
func isLlamaCppEngine(name string) bool {
	switch name {
	case "llamacpp", "llama-cpp", "llamacpp-rpc":
		return true
	}
	return false
}

// parseEndpointPort extracts the port from "http://host:port" / "host:port".
// Returns 0 when the input lacks an explicit port — callers must error out
// before passing 0 to the supervisor.
func parseEndpointPort(endpoint string) int {
	if u, err := url.Parse(endpoint); err == nil && u.Port() != "" {
		if p, err := strconv.Atoi(u.Port()); err == nil {
			return p
		}
	}
	return 0
}

// firstRunWizard prompts the user whether to install the auto-picked
// starter model on first boot. Returns true if the wizard ran (handled
// the install OR the user explicitly declined); false to signal the
// caller should fall through to the silent `ensureDefaultModel` path.
//
// Triggers only when stdin is interactive AND no models are installed
// yet. Otherwise it's a no-op so scripted/CI uses get the existing
// silent auto-pull behavior.
func firstRunWizard(cfg *config.Config, cat []models.Entry, st store.Store, eng engines.Engine, caps agent.Capabilities) bool {
	// Non-interactive context (CI, piped stdin) — skip and let the
	// silent path run.
	if !isInteractiveStdin() {
		return false
	}
	// Anything already installed → not a first run.
	if ms, err := st.Models().List(context.Background()); err == nil && len(ms) > 0 {
		return false
	}
	pick := models.FindByID(cat, cfg.Router.DefaultModel)
	if pick == nil {
		return false
	}
	bold, dim, reset := ansiCodes()
	fmt.Println()
	fmt.Printf("  %sFirst run.%s Pick a starter model to install — chat works as soon as it's done.\n", bold, reset)
	fmt.Printf("  %sRecommended for your hardware (%d GB RAM):%s %s — %s %s(%.1f GB)%s\n",
		dim, caps.RAMGB, reset, pick.ID, pick.DisplayName, dim, float64(pick.SizeBytes)/1e9, reset)
	fmt.Printf("  Install %s now? [%sY%s/n/o=pick another] ", pick.ID, bold, reset)
	r := bufio.NewReader(os.Stdin)
	line, _ := r.ReadString('\n')
	ans := strings.ToLower(strings.TrimSpace(line))
	switch ans {
	case "n", "no":
		note(os.Stdout, "skipped — install later with `opod model add <id>`")
		return true
	case "o", "other", "pick", "p":
		chosen := pickCatalogID("Pick a model to install:", "")
		if chosen == "" {
			note(os.Stdout, "skipped — install later with `opod model add <id>`")
			return true
		}
		cfg.Router.DefaultModel = chosen
	}
	ensureDefaultModel(cfg, cat, st, eng)
	return true
}

// isInteractiveStdin reports whether stdin is wired up to a real TTY.
// Used to gate interactive prompts so scripts don't hang waiting for
// input that will never come.
func isInteractiveStdin() bool {
	return isatty.IsTerminal(os.Stdin.Fd())
}

// engineStartHint returns the copy-pasteable command that brings the
// configured engine up. Used by `opod up` and `opod doctor` when the
// engine health probe fails.
func engineStartHint(engineName string) string {
	return engines.StartHint(engineName)
}

// printReturningAdminKey is the banner's admin-key lines for a leader whose
// key already exists. It says WHERE the key is, never the key: under systemd
// or Kubernetes stdout is a log, and printing the key on every start put it
// in every log line it ever reached (PLAN T10.17). The key is printed once,
// by the start that created it.
func printReturningAdminKey(w io.Writer, path string, saved bool) {
	fmt.Fprintln(w)
	if saved {
		fmt.Fprintf(w, "  Admin API key:  saved in %s (read it with: cat %s)\n", path, path)
		fmt.Fprintln(w, "    →  Mint another admin key:  opod token create ops --admin")
	} else {
		fmt.Fprintf(w, "  Admin API key:  not saved on this host (%s missing).\n", path)
		fmt.Fprintln(w, "    →  Mint a new one:  opod token create ops --admin")
		fmt.Fprintln(w, "    →  Old keys can't be recovered (DB stores hashes only).")
	}
	fmt.Fprintln(w)
}
