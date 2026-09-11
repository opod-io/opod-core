package main

// `opod up` — the ready banner, network posture and first-run wizard.

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"bufio"
	"net/url"

	"github.com/mattn/go-isatty"
	"github.com/opod-io/opod/internal/agent"
	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/models"
	"github.com/opod-io/opod/internal/store"
)

func printReady(cfg *config.Config, adminKey string) {
	listen := cfg.Listen
	if listen == "" {
		listen = ":8080"
	}
	// ":8080" → http://localhost:8080; "127.0.0.1:8080" → http://127.0.0.1:8080
	base := "http://" + listen
	if strings.HasPrefix(listen, ":") {
		base = "http://localhost" + listen
	}
	if cfg.ExternalURL != "" {
		base = cfg.ExternalURL
	}
	fmt.Println()
	fmt.Println("  Opod is ready.")
	fmt.Println()
	fmt.Printf("  Dashboard:  %s\n", base)
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
		nx("Test in the browser:", base)
		nx("Wire up Claude Code:", "opod connect claude-code")
		nx("Wire up Cursor:", "opod connect cursor")
		nx("See all clients:", "opod connect --list")
		nx("Mint another admin key:", "opod token create dashboard --admin")
		fmt.Println()
		fmt.Println("  Quick test from the shell:")
		fmt.Printf("    curl %s/v1/chat/completions \\\n", base)
		fmt.Printf("      -H 'Authorization: Bearer %s' \\\n", adminKey)
		fmt.Println(`      -H 'Content-Type: application/json' \`)
		fmt.Printf("      -d '{\"model\":\"%s\",\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}]}'\n", cfg.Router.DefaultModel)
	} else {
		// Returning user: a key already lives in the DB. Re-display it from
		// ~/.opod/admin.key if we still have it on disk so the operator
		// doesn't have to dig — that file is where `opod connect` reads
		// from too. If it's missing (older install, deleted, multi-host),
		// point at `opod token create` rather than silently dropping the
		// dashboard/curl hints.
		saved := readLocalAdminKey(cfg)
		fmt.Println()
		if saved != "" {
			fmt.Printf("  Admin API key (from %s):\n", localAdminKeyPath(cfg))
			fmt.Printf("    %s\n", saved)
			fmt.Println("    →  Mint another admin key:  opod token create dashboard --admin")
			fmt.Println()
		} else {
			fmt.Printf("  Admin API key:  not saved on this host (%s missing).\n", localAdminKeyPath(cfg))
			fmt.Println("    →  Mint a new one:  opod token create dashboard --admin")
			fmt.Println("    →  Old keys can't be recovered (DB stores hashes only).")
			fmt.Println()
		}
		fmt.Println("  Wire up a tool:")
		fmt.Println("    opod connect claude-code   # or: cursor, aider, continue, …")
		fmt.Println("    opod connect --list        # see all supported clients")
	}
	printNetworkPosture(cfg)
	fmt.Println()
	fmt.Println("  Press Ctrl-C to stop.")
	fmt.Println()
}

// printNetworkPosture lists every outbound network call Opod can make
// from this process. Designed so an operator running `opod up` can audit
// the privacy posture at a glance — no surprises, no silent calls. Reads
// the live config so it reflects what *this* invocation will actually do.
func printNetworkPosture(cfg *config.Config) {
	fmt.Println()
	fmt.Println("  Surfaces:        " + cfg.Surfaces.Summary() + "  (OPOD_UI / OPOD_EGRESS / OPOD_PROTOCOLS / OPOD_CALLBACKS / OPOD_MANAGED)")
	fmt.Println()
	fmt.Println("  Network behavior on this node:")

	// Tracing
	if cfg.Observability.OTLPEndpoint == "" {
		fmt.Println("    · Tracing:       OFF  (set OPOD_OTLP_ENDPOINT=… to your collector to enable)")
	} else {
		fmt.Printf("    · Tracing:       → %s  (your collector; set OFF by clearing OPOD_OTLP_ENDPOINT)\n", cfg.Observability.OTLPEndpoint)
	}

	fmt.Println("    · Telemetry:     none. Opod never reports installs, usage, errors, or any data to opod.io.")
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
