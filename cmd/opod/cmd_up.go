package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/opod-io/opod/internal/agent"
	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/control"
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
	plainKey := ""
	if cfg.Auth.AdminToken == "" {
		plainKey = bootstrapAdminKey(st, cfg) // standalone: mint + save the first admin key
	} // managed: the configured token is seeded below; nothing is written to disk
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
	plain, err := control.BootstrapAdminKey(context.Background(), st)
	if err != nil {
		warn(os.Stdout, "could not bootstrap admin key: %v — check `opod token ls`", err)
		return ""
	}
	if plain == "" {
		return ""
	}
	// Save the plaintext to ~/.opod/admin.key (0600) so later CLI runs on this
	// host authenticate without the user remembering it (same trust model as
	// ~/.aws/credentials).
	path := localAdminKeyPath(cfg)
	if err := os.WriteFile(path, []byte(plain), 0o600); err != nil {
		warn(os.Stdout, "could not save admin key to %s: %v", path, err)
	}
	return plain
}

// seedJoinToken makes the configured node-join token valid on this leader
// (control.SeedJoinToken); prints what happened.
func seedJoinToken(st store.Store, cfg *config.Config) {
	out, err := control.SeedJoinToken(context.Background(), st, cfg)
	switch {
	case err != nil:
		warn(os.Stdout, "%v", err)
	case out.Seeded:
		note(os.Stdout, "seeded node-join token — workers holding the configured join token can join this leader")
	case out.Hint != "":
		note(os.Stdout, "%s", out.Hint)
	}
}

// seedAdminToken mirrors seedJoinToken for the manager's admin token.
func seedAdminToken(st store.Store, cfg *config.Config) {
	out, err := control.SeedAdminToken(context.Background(), st, cfg)
	switch {
	case err != nil:
		warn(os.Stdout, "%v", err)
	case out.Seeded:
		note(os.Stdout, "seeded admin token — the configured manager token can call /admin/v1")
	}
}

// ensureDefaultModel makes the default model resident (control.EnsureDefaultModel)
// with a progress bar; failures are logged, never fatal at boot.
func ensureDefaultModel(cfg *config.Config, cat []models.Entry, st store.Store, eng engines.Engine) {
	bar := newProgressBar("pulling " + cfg.Router.DefaultModel)
	out, err := control.EnsureDefaultModel(context.Background(), cfg, cat, st, eng, bar.update)
	bar.done()
	if err != nil {
		warn(os.Stdout, "%v", err)
		return
	}
	if out.Pulled {
		ok(os.Stdout, "model ready: %s", cfg.Router.DefaultModel)
	}
}
