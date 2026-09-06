package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mattn/go-isatty"

	"github.com/opod-io/opod/internal/agent"
	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/controlplane"
	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/lifecycle"
	"github.com/opod-io/opod/internal/models"
	"github.com/opod-io/opod/internal/scheduler"
	"github.com/opod-io/opod/internal/store"
)

func cmdUp(args []string) {
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	configPath := fs.String("config", "", "path to config.yaml (default: ~/.opod/config.yaml)")
	autoPull := fs.Bool("auto-pull", true, "auto-pull the default model on first run")
	noWizard := fs.Bool("no-wizard", false, "skip the interactive first-run prompt; combine with --auto-pull=false for a fully quiet boot")
	unloadOnExit := fs.Bool("unload-on-exit", os.Getenv("OPOD_UNLOAD_ON_EXIT") == "1",
		"on Ctrl-C, ask the engine to drop loaded models from RAM (OPOD_UNLOAD_ON_EXIT=1 sets the default)")
	exclusive := fs.Bool("exclusive", false,
		"one resident model per machine: loading a model evicts every other non-pinned model first (OPOD_EXCLUSIVE=1 or placement.exclusive in config also set this)")
	help := helpSpec{
		name:    "up",
		summary: "start the local node (becomes the cluster leader on first run)",
		usage:   "opod up [--config <path>] [--auto-pull=false] [--no-wizard] [--exclusive]",
		flags:   fs,
		examples: []string{
			"opod up",
			"OPOD_DEFAULT_MODEL=llama-3.2-1b opod up",
			"OPOD_ENGINE=llamacpp opod up           # auto-spawns llama-server if not already running",
			"opod up --config ~/.opod/staging.yaml",
			"opod up --auto-pull=false              # don't pre-pull the default model",
			"opod up --no-wizard                    # skip the interactive 'install a starter?' prompt",
		},
		notes: []string{
			"On first run, prints an admin API key — save it. Subsequent runs reuse the saved key.",
			"When engine.preferred=llamacpp and no llama-server is listening on engine.llamacpp_endpoint, Opod auto-launches `llama-server -hf <repo>` for the default model (if its catalog entry has source.repo set) and stops it again on shutdown.",
		},
	}
	// Bad flags: print usage to stderr and let ExitOnError exit 2.
	fs.Usage = func() { showUsageErr(help) }
	if wantsHelp(args) {
		showHelp(help)
	}
	_ = fs.Parse(args)

	// 1. Config + logger
	cfg, err := config.Load(*configPath)
	if err != nil {
		die("config: %v", err)
	}
	log := newLogger(cfg)

	// 2. Hardware detection
	caps := agent.Detect()
	note(os.Stdout, "detected %s/%s · %d GB RAM · %d cores",
		caps.OS, caps.Arch, caps.RAMGB, caps.CPUCores)

	// 3. Store
	st := openStoreOrExit(cfg)
	defer st.Close()

	// 4. Bootstrap admin key on first run
	plainKey := bootstrapAdminKey(st, cfg)
	seedJoinToken(st, cfg)
	seedAdminToken(st, cfg)

	// 5. Catalog + auto-pick default model
	cat := loadCatalogOrExit(cfg)
	if cfg.Router.DefaultModel == "" {
		if pick, found := models.AutoPick(cat, caps, 4); found {
			cfg.Router.DefaultModel = pick.ID
			ok(os.Stdout, "auto-selected model: %s (%s)", pick.ID, pick.DisplayName)
		} else {
			warn(os.Stdout, "no catalog entry fits this hardware; set router.default_model in config")
		}
	} else {
		ok(os.Stdout, "default model: %s", cfg.Router.DefaultModel)
	}

	// 6. Process supervisor — created early so it can also auto-spawn
	//    llama-server below when engine.preferred=llamacpp. Used later by
	//    the sharding orchestrator for the coordinator on sharded models.
	sup := agent.NewSupervisor(log)
	defer sup.StopAll()

	// 7. Engine — bounded health probe so a wedged-but-listening engine
	//    doesn't block startup indefinitely.
	eng := newEngineFromConfig(cfg)
	healthCtx, healthCancel := context.WithTimeout(context.Background(), 3*time.Second)
	engineOK := eng.Health(healthCtx) == nil
	healthCancel()

	// 7a. Auto-spawn llama-server when the user picked the llamacpp engine
	//     and there's nothing listening yet. Keeps the UX symmetric with
	//     `ollama serve` running in the background. Skipped for the other
	//     engines for now — vLLM and MLX-LM both have heavier launch
	//     surfaces (Python deps, GPU allocation flags) that warrant explicit
	//     user control.
	if !engineOK && isLlamaCppEngine(eng.Name()) && cfg.Router.DefaultModel != "" && cfg.Router.PullDefaultModel {
		if entry := models.FindByID(cat, cfg.Router.DefaultModel); entry != nil {
			port := parseEndpointPort(cfg.Engine.LlamaCppEndpoint)
			spawnCtx, spawnCancel := context.WithTimeout(context.Background(), 5*time.Minute)
			note(os.Stdout, "auto-spawning llama-server for %s on :%d ...", entry.ID, port)
			spec := scheduler.LlamaCppLaunchSpec{Entry: entry, Port: port}
			_, err := scheduler.EnsureLlamaServer(spawnCtx, sup, log, spec)
			spawnCancel()
			if err != nil {
				warn(os.Stdout, "auto-spawn failed: %v", err)
			} else {
				reprobeCtx, reprobeCancel := context.WithTimeout(context.Background(), 10*time.Second)
				engineOK = eng.Health(reprobeCtx) == nil
				reprobeCancel()
				if engineOK {
					ok(os.Stdout, "llama-server ready (auto-spawned)")
					// Spawned engines get a health watchdog that force-
					// restarts the process if it ever becomes unresponsive
					// (the supervisor's crash-restart only covers actual
					// process exit; this also covers hung llama-server).
					scheduler.StartHealthWatchdog(context.Background(), sup, log, eng, spec)
				}
			}
		}
	}

	// Register the leader as a "local" Node row so `opod node ls` and the
	// admin UI show this machine alongside any joined workers. Best-effort.
	{
		listen := cfg.Listen
		if listen == "" {
			listen = ":8080"
		}
		hwJSON, _ := json.Marshal(caps)
		_ = st.Nodes().Upsert(context.Background(), store.Node{
			ID:            "local",
			Hostname:      caps.Hostname,
			OS:            caps.OS,
			Arch:          caps.Arch,
			RAMGB:         caps.RAMGB,
			Address:       "127.0.0.1" + listen,
			HardwareJSON:  string(hwJSON),
			LastHeartbeat: time.Now(),
			State:         "ready",
		})
	}

	// Sync any locally-loaded models into placements so the Router knows
	// the leader's own engine has them. Best-effort.
	if engineOK {
		listCtx, listCancel := context.WithTimeout(context.Background(), 3*time.Second)
		if loaded, lerr := eng.List(listCtx); lerr == nil {
			for _, m := range loaded {
				_ = st.Placements().Upsert(listCtx, store.Placement{
					NodeID: "local", ModelID: m, Status: "ready", LastSeen: time.Now(),
				})
			}
		}
		listCancel()
	}
	if !engineOK {
		warn(os.Stdout, "engine (%s) at %s is not reachable", eng.Name(), eng.Endpoint())
		warn(os.Stdout, "  → %s", engineStartHint(eng.Name()))
		warn(os.Stdout, "  then check `opod status`")
	} else {
		ok(os.Stdout, "engine: %s at %s", eng.Name(), eng.Endpoint())
		if *autoPull && cfg.Router.DefaultModel != "" && cfg.Router.PullDefaultModel {
			if !*noWizard && firstRunWizard(cfg, cat, st, eng, caps) {
				// Wizard handled the install (or the user declined). Either
				// way, skip the silent auto-pull below so we don't double up.
			} else {
				ensureDefaultModel(cfg, cat, st, eng)
			}
		}
	}

	// 8. Persist PID
	if err := writePID(cfg); err != nil {
		warn(os.Stdout, "could not write PID file: %v", err)
	}
	defer removePID(cfg)

	// 9. Print ready block
	printReady(cfg, plainKey)

	// 10. Sharding orchestrator (uses the supervisor created at step 6;
	//     leader runs the coordinator llama-server for sharded models).
	orch := scheduler.New(st, sup, log, cfg.Storage.ModelsDir)

	// 11. Start server with signal context
	srv := controlplane.NewServer(cfg, st, eng, cat, log, orch)
	srv.Version = version

	// 11a. Memory-lifecycle manager: admission ("does it fit?"), LRU
	//      evict-and-swap, and desired-placement restore for the local
	//      engine. AttachLifecycle wires it to the router's in-flight
	//      counts so evictions drain before unloading.
	mgr := lifecycle.New(st, eng, cat, caps.RAMGB, log)
	mgr.Exclusive = cfg.Placement.Exclusive || *exclusive
	if cfg.Placement.ReservePercent > 0 {
		mgr.ReservePercent = cfg.Placement.ReservePercent
	}
	if cfg.Placement.DrainTimeoutSeconds > 0 {
		mgr.DrainTimeout = time.Duration(cfg.Placement.DrainTimeoutSeconds) * time.Second
	}
	srv.AttachLifecycle(mgr)
	if mgr.Exclusive {
		note(os.Stdout, "exclusive placement: loading a model evicts every other non-pinned model")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// 11b. Restore desired placements (models the operator loaded/pinned
	//      before the last shutdown) in the background — a 40 GB warm
	//      load must not block the gateway from serving.
	if engineOK {
		go mgr.Restore(ctx)
	}

	if err := srv.Start(ctx); err != nil {
		die("server: %v", err)
	}

	// Optional: ask the engine to drop every model it has resident in RAM
	// before we exit. The serve context has already been cancelled (that's
	// why srv.Start returned), so we use fresh bounded contexts. Best-
	// effort — failures degrade to a soft warning, not a non-zero exit.
	//
	// For engines that don't support online unload (llamacpp / vLLM /
	// MLX-LM), this is a silent no-op: when Opod auto-spawned them, the
	// deferred sup.StopAll() (registered earlier and run AFTER this block
	// per LIFO defer order) kills the process and frees the RAM; when the
	// user manages them, Opod has no business calling restart on their
	// behalf. Either way the "restart it to free RAM" hint would be
	// misleading in this exit path.
	if *unloadOnExit {
		uCtx, uCancel := context.WithTimeout(context.Background(), 10*time.Second)
		loaded, listErr := eng.List(uCtx)
		if listErr != nil {
			warn(os.Stdout, "unload-on-exit: could not list models (%v)", listErr)
			uCancel()
		} else {
			var unloaded, skipped int
			var firstErr error
			for _, m := range loaded {
				err := eng.Unload(uCtx, m)
				switch {
				case err == nil:
					unloaded++
				case errors.Is(err, engines.ErrUnloadNotSupported):
					skipped++
				default:
					if firstErr == nil {
						firstErr = err
					}
				}
			}
			uCancel()
			switch {
			case firstErr == nil && unloaded > 0:
				ok(os.Stdout, "unloaded %d model(s) from %s", unloaded, eng.Name())
			case firstErr != nil:
				warn(os.Stdout, "unload-on-exit: %v (succeeded for %d)", firstErr, unloaded)
			case skipped > 0:
				// All models were on a non-supporting engine — say nothing;
				// sup.StopAll() (LIFO defer) will free Opod-owned engines.
			}
		}
	}

	ok(os.Stdout, "shutdown complete")
}

func bootstrapAdminKey(st store.Store, cfg *config.Config) string {
	ctx := context.Background()
	keys, err := st.APIKeys().List(ctx)
	if err != nil {
		warn(os.Stdout, "could not list api keys (%v) — skipping bootstrap; check `opod token ls`", err)
		return ""
	}
	if len(keys) > 0 {
		return ""
	}
	plain, rec, err := auth.Generate("initial-admin", "admin", "admin")
	if err != nil {
		warn(os.Stdout, "could not bootstrap admin key: %v", err)
		return ""
	}
	if err := st.APIKeys().Create(ctx, rec); err != nil {
		warn(os.Stdout, "could not persist admin key: %v", err)
		return ""
	}
	// Save plaintext to ~/.opod/admin.key (mode 0600) so subsequent CLI
	// invocations on this host can authenticate to the leader without the
	// user having to remember the key. Same trust model as ~/.aws/credentials.
	path := localAdminKeyPath(cfg)
	if err := os.WriteFile(path, []byte(plain), 0o600); err != nil {
		warn(os.Stdout, "could not save admin key to %s: %v", path, err)
	}
	return plain
}

// seedJoinToken makes a CONFIGURED node-join token valid on THIS leader by
// ensuring a matching node-scoped api_keys row exists. Because the store keeps
// only the sha256 hash of a key, a KNOWN token can be seeded deterministically:
// a leader with a fresh/wiped DB (new host, different OPOD_DATA_DIR, ephemeral
// container) will then ACCEPT workers holding cfg.Auth.JoinToken instead of
// 401'ing them and making them vanish — which is what lets worker joins survive
// a leader restart or rebuild. Idempotent: the row is keyed by the token's own
// hash, so restarting the leader never duplicates or churns it. No-op when no
// join token is configured (auth.join_token / OPOD_JOIN_TOKEN empty), preserving
// the manual `opod token create --node` workflow.
func seedJoinToken(st store.Store, cfg *config.Config) {
	token := strings.TrimSpace(cfg.Auth.JoinToken)
	if token == "" {
		// No shared join token configured. If the leader also has no live
		// node-scoped key, nothing can join yet — nudge the operator toward the
		// flow instead of leaving a worker to fail with a silent 401. Stays quiet
		// once any node token exists (manual `opod token create --node` workflow).
		if keys, err := st.APIKeys().List(context.Background()); err == nil {
			for _, k := range keys {
				if k.Scope == "node" && !k.Revoked {
					return
				}
			}
			note(os.Stdout, "no node-join token yet — run `opod token create --node` (or set OPOD_JOIN_TOKEN) before `opod join` on a worker")
		}
		return
	}
	ctx := context.Background()
	hash := auth.Hash(token)
	if existing, err := st.APIKeys().GetByHash(ctx, hash); err == nil && existing != nil && !existing.Revoked {
		return // already seeded and live — nothing to do
	}
	// Deterministic id derived from the hash so the SAME token always maps to the
	// SAME key id. That keeps the leader's first-use node binding (BoundKeyID)
	// stable across restarts — a rejoining worker isn't 403'd as "bound to a
	// different key" the way a freshly-random token id would cause.
	rec := store.APIKey{
		ID:        "k_seed_" + hash[:12],
		Hash:      hash,
		Name:      "fleet-join (seeded)",
		Scope:     "node",
		CreatedAt: time.Now(),
	}
	if err := st.APIKeys().Create(ctx, rec); err != nil {
		warn(os.Stdout, "could not seed node-join token: %v", err)
		return
	}
	note(os.Stdout, "seeded node-join token — workers holding the configured join token can join this leader")
}

// seedAdminToken mirrors seedJoinToken for an ADMIN-scoped key: a leader
// provisioned by an external manager accepts that manager's configured token
// on /admin/v1 even with a fresh/wiped DB. Idempotent; no-op when unset.
func seedAdminToken(st store.Store, cfg *config.Config) {
	token := strings.TrimSpace(cfg.Auth.AdminToken)
	if token == "" {
		return
	}
	ctx := context.Background()
	hash := auth.Hash(token)
	if existing, err := st.APIKeys().GetByHash(ctx, hash); err == nil && existing != nil && !existing.Revoked {
		return
	}
	rec := store.APIKey{
		ID:        "k_seedadm_" + hash[:12],
		Hash:      hash,
		Name:      "manager-admin (seeded)",
		Scope:     "admin",
		CreatedAt: time.Now(),
	}
	if err := st.APIKeys().Create(ctx, rec); err != nil {
		warn(os.Stdout, "could not seed admin token: %v", err)
		return
	}
	note(os.Stdout, "seeded admin token — the configured manager token can call /admin/v1")
}

// ensureDefaultModel records the default model in the store and triggers a pull
// if the engine doesn't already have it. Best-effort; failures are logged but
// don't block startup.
func ensureDefaultModel(cfg *config.Config, cat []models.Entry, st store.Store, eng engines.Engine) {
	entry := models.FindByID(cat, cfg.Router.DefaultModel)
	if entry == nil {
		warn(os.Stdout, "default model %q not found in catalog", cfg.Router.DefaultModel)
		return
	}
	engineModelName := entry.Source.OllamaName
	if engineModelName == "" {
		engineModelName = entry.ID
	}
	// Already pulled?
	existing, _ := eng.List(context.Background())
	for _, m := range existing {
		if m == engineModelName {
			_ = st.Models().Upsert(context.Background(), store.Model{
				ID: entry.ID, CatalogID: entry.ID,
				Source: "ollama:" + engineModelName, Status: "ready",
				SizeBytes: entry.SizeBytes, InstalledAt: time.Now(),
			})
			return
		}
	}
	note(os.Stdout, "pulling %s ...", engineModelName)
	bar := newProgressBar("pulling " + entry.ID)
	err := eng.Pull(context.Background(), engineModelName, bar.update)
	bar.done()
	if err != nil {
		warn(os.Stdout, "pull failed: %v", err)
		return
	}
	_ = st.Models().Upsert(context.Background(), store.Model{
		ID: entry.ID, CatalogID: entry.ID,
		Source: "ollama:" + engineModelName, Status: "ready",
		SizeBytes: entry.SizeBytes, InstalledAt: time.Now(),
	})
	ok(os.Stdout, "model ready: %s", entry.ID)
}

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
		nx("Invite a teammate:", "opod invite <name>")
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
	MaybeShowUpdateNotice(os.Stdout)
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

	// Update check
	if cfg.Surfaces.Managed {
		fmt.Println("    · Update check:  OFF  (managed by an external manager — OPOD_MANAGED=1)")
	} else if os.Getenv("OPOD_NO_UPDATE_CHECK") == "1" {
		fmt.Println("    · Update check:  OFF  (OPOD_NO_UPDATE_CHECK=1)")
	} else {
		fmt.Println("    · Update check:  github.com/opod-io/opod/releases/latest, max 1× per 24h  (OPOD_NO_UPDATE_CHECK=1 to disable)")
	}

	// Vendor egress — only print rows where the operator opted in.
	if cfg.Router.Fallback.AnthropicKey != "" {
		fmt.Println("    · Anthropic:     → api.anthropic.com on claude-* requests  (ANTHROPIC_API_KEY set)")
	}
	if cfg.Router.Fallback.OpenAIKey != "" {
		fmt.Println("    · OpenAI:        → api.openai.com on gpt-*/o-* requests  (OPENAI_API_KEY set)")
	}
	if cfg.Router.Fallback.BedrockRegion != "" {
		fmt.Printf("    · Bedrock:       → bedrock-runtime.%s.amazonaws.com on anthropic.* (SigV4 via AWS chain)\n", cfg.Router.Fallback.BedrockRegion)
	}
	if cfg.Router.Fallback.VertexProject != "" {
		fmt.Printf("    · Vertex:        → %s-aiplatform.googleapis.com (ADC for project %s)\n", orDefault(cfg.Router.Fallback.VertexLocation, "us-central1"), cfg.Router.Fallback.VertexProject)
	}

	fmt.Println("    · Telemetry:     none. Opod never reports installs, usage, errors, or any data to opod.io.")
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
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
