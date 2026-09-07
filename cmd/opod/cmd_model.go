package main

// `opod model` — dispatch, pickers and the list/remove subcommands.
// Install/search/info/load live in cmd_model_{add,info,load}.go; the
// logic behind them is internal/models (P13-9).

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/opod-io/opod/internal/models"
	"github.com/opod-io/opod/internal/store"
)

func cmdModel(args []string) {
	help := helpSpec{
		name:    "model",
		summary: "install, list, search, inspect, load/unload, or uninstall LLM models",
		usage:   "opod model <add <id> [--force] [--node a,b,c] | ls | ps | search [query] | info <id> | load <id> [--swap] [--pin] [--priority N] | unload <id> | remove <id>>",
		examples: []string{
			"opod model search                # browse the full catalog",
			"opod model search coder          # filter to coding models",
			"opod model search vision         # filter by capability (vision, embedding, tools, …)",
			"opod model search --sort=released         # newest models first",
			"opod model search --since 2026-01-01      # only models released since",
			"opod model info qwen-coder-14b   # full details: capabilities, hardware, fallback chain, install + use snippets",
			"opod model add llama-3.2-3b      # install (auto-delegates if sharded)",
			"opod model add llama-3.3-70b --force      # bypass the hardware floor check",
			"opod model add qwen3.6-27b --dry-run      # preview download size, RAM, engine — no pull",
			"opod model add hf:bartowski/Phi-3-mini-GGUF   # any HuggingFace repo (skips catalog)",
			"opod model add ollama:phi3:mini  # any Ollama tag (must be using ollama engine)",
			"opod model add file:/tmp/my.gguf # a pre-downloaded GGUF on disk",
			"opod model add --from ./my-model.yaml   # install from a user-supplied catalog YAML",
			"opod model add llama-3.2-3b --node gpu-a         # pin to one worker (it pulls + loads)",
			"opod model add llama-3.2-3b --nodes gpu-a,gpu-b # pin to several workers",
			"opod model ls                    # list installed models",
			"opod model ps                    # models resident in engine RAM + free memory",
			"opod model load qwen-coder-14b   # bring into RAM now (refuses if it doesn't fit)",
			"opod model load qwen-coder-14b --swap     # evict least-recently-used models to make room",
			"opod model load nomic-embed-text --pin    # exempt from eviction + engine idle TTL",
			"opod model remove llama-3.2-3b   # uninstall (prompts; pass --yes to skip)",
			"opod model unload llama-3.2-3b   # drop from engine RAM without deleting weights",
		},
		notes: []string{
			"`add` refuses if the catalog's min_ram_gb / min_vram_gb exceeds detected hardware.",
			"Override with --force when you know swap, quantization, or sharding will compensate.",
			"`add` also HEAD-checks the upstream (Ollama registry / HuggingFace) and refuses on a 404 — so a typo'd hf:owner/repo fails here, not at engine launch. Network trouble only warns. OPOD_SKIP_SOURCE_CHECK=1 skips the probe (air-gapped mirrors).",
			"`load` is memory-aware: it checks live engine residency (Ollama /api/ps) against this machine's RAM budget and refuses rather than overcommit; `--swap` evicts least-recently-used, non-pinned models (drained first, audit-logged). Loaded/pinned models are restored on the next `opod up`.",
			"`--node a,b,c` pins a (non-sharded) model to specific workers: the leader tells each worker to pull + load it, and it appears as placed once the worker's next heartbeat reports it. Without --node, `add` installs to the leader's local engine.",
			"For sharded models (split across multiple machines) see `opod shard --help`.",
			"For the complete per-model walkthrough see MODELS.md in the repo.",
			"Adding a model not in the catalog: use a scheme prefix (`hf:owner/repo`, `ollama:tag`, `file:/abs/path.gguf`) for a one-liner, `--from <my.yaml>` to install from your own catalog entry, or drop a YAML file into `~/.opod/catalog/` and run `opod model add <id>`.",
		},
	}
	if len(args) == 0 {
		dieHelp(help)
	}
	if wantsHelp(args) {
		showHelp(help)
	}
	switch args[0] {
	case "add":
		id, force, dryRun, fromPath, nodes := parseModelAddArgs(args[1:])
		// `--from <my.yaml>` installs a model from a user-supplied catalog
		// YAML, copying it into `~/.opod/catalog/` so it persists and
		// shows up in `opod model search` / `info` next time.
		if fromPath != "" {
			modelAddFromYAML(fromPath, force, dryRun)
			return
		}
		// `--node a,b,c` pins the model to specific workers: the running leader
		// tells each worker to pull + load it (POST /admin/v1/models {nodes}).
		// Goes through the leader because only the live server holds the worker
		// addresses + tokens to dial them.
		if len(nodes) > 0 {
			if dryRun {
				die("--dry-run is not supported with --node")
			}
			if _, ok := models.ParseSchemeID(id); !ok {
				if id == "" || !catalogHasID(id) {
					id = pickCatalogID("Pick a model to place:", id)
					if id == "" {
						die("no model selected")
					}
				}
			}
			modelAddOnNodes(id, nodes)
			return
		}
		// Scheme-prefixed ids (hf:, ollama:, file:) skip the catalog lookup
		// entirely — they describe a model we know how to install but have
		// no curated YAML for. Hardware-floor and dry-run plans both fall
		// through to the same engine pull.
		if entry, ok := models.ParseSchemeID(id); ok {
			if dryRun {
				modelAddDryRun(id)
				return
			}
			modelAddEntry(entry, force)
			return
		}
		if id == "" || !catalogHasID(id) {
			id = pickCatalogID("Pick a model to install:", id)
			if id == "" {
				die("no model selected")
			}
		}
		if dryRun {
			modelAddDryRun(id)
			return
		}
		modelAdd(id, force)
	case "ls", "list":
		_, asJSON := extractJSONFlag(args[1:])
		modelLs(asJSON)
	case "remove", "rm":
		rest, yes := extractYesFlag(args[1:])
		id := ""
		if len(rest) >= 1 {
			id = rest[0]
		}
		if id == "" || !installedHasID(id) {
			id = pickInstalledID("Pick an installed model to remove:", id)
			if id == "" {
				die("no model selected")
			}
		}
		if !yes && !confirm(fmt.Sprintf("Remove installed model %q? Weights will be deleted from disk. (y/N) ", id)) {
			die("aborted")
		}
		modelRemove(id)
	case "search":
		rest, asJSON := extractJSONFlag(args[1:])
		query, sortReleased, since := parseModelSearchArgs(rest)
		modelSearch(query, sortReleased, since, asJSON)
	case "info":
		rest, asJSON := extractJSONFlag(args[1:])
		id := ""
		if len(rest) >= 1 {
			id = rest[0]
		}
		if id == "" || !catalogHasID(id) {
			id = pickCatalogID("Pick a model to inspect:", id)
			if id == "" {
				die("no model selected")
			}
		}
		modelInfo(id, asJSON)
	case "unload":
		id := ""
		if len(args) >= 2 {
			id = args[1]
		}
		if id == "" || !installedHasID(id) {
			id = pickInstalledID("Pick an installed model to unload:", id)
			if id == "" {
				die("no model selected")
			}
		}
		modelUnload(id)
	case "load":
		rest := args[1:]
		id := ""
		var loadArgs []string
		for i := 0; i < len(rest); i++ {
			a := rest[i]
			if !strings.HasPrefix(a, "-") && id == "" {
				id = a
				continue
			}
			loadArgs = append(loadArgs, a)
			// --priority takes a value: keep the next token attached to
			// the flag so `model load --priority 3 qwen3-14b` doesn't
			// mistake "3" for the positional model id.
			if (a == "--priority" || a == "-priority") && i+1 < len(rest) {
				loadArgs = append(loadArgs, rest[i+1])
				i++
			}
		}
		if id == "" || !installedHasID(id) {
			id = pickInstalledID("Pick an installed model to load:", id)
			if id == "" {
				die("no model selected")
			}
		}
		modelLoad(id, loadArgs)
	case "ps":
		_, asJSON := extractJSONFlag(args[1:])
		modelPs(asJSON)
	default:
		dieUnknownSubcommand("model", args[0], []string{"add", "ls", "remove", "search", "info", "load", "unload", "ps"})
	}
}

// catalogHasID is a cheap membership check used to decide whether to fall
// through to the picker.
func catalogHasID(id string) bool {
	if id == "" {
		return false
	}
	cfg := loadConfigOrExit()
	cat, err := models.LoadCatalog(cfg.CatalogDir)
	if err != nil {
		return false
	}
	return models.FindByID(cat, id) != nil
}

// installedHasID returns true if `id` is currently installed in the store.
func installedHasID(id string) bool {
	if id == "" {
		return false
	}
	cfg := loadConfigOrExit()
	st, err := store.OpenSQLite(cfg.Storage.DSN)
	if err != nil {
		return false
	}
	defer st.Close()
	m, _ := st.Models().Get(context.Background(), id)
	return m != nil
}

// pickCatalogID launches the interactive picker over all catalog entries.
// Returns the chosen ID or "" if the user cancelled / stdin isn't a TTY.
func pickCatalogID(prompt, seed string) string {
	cfg := loadConfigOrExit()
	cat := loadCatalogOrExit(cfg)
	items := make([]pickerItem, 0, len(cat))
	for _, e := range cat {
		meta := strings.Join(e.Capabilities, ",")
		if e.SizeBytes > 0 {
			meta = fmt.Sprintf("%.1f GB · %s", float64(e.SizeBytes)/1e9, meta)
		}
		if e.License != "" {
			meta += " · " + e.License
		}
		items = append(items, pickerItem{ID: e.ID, Label: e.DisplayName, Meta: meta})
	}
	return pickFromList(prompt, items, seed)
}

// pickInstalledID launches the picker scoped to installed models only.
func pickInstalledID(prompt, seed string) string {
	cfg := loadConfigOrExit()
	st, err := store.OpenSQLite(cfg.Storage.DSN)
	if err != nil {
		return ""
	}
	defer st.Close()
	rows, err := st.Models().List(context.Background())
	if err != nil || len(rows) == 0 {
		return ""
	}
	items := make([]pickerItem, 0, len(rows))
	for _, m := range rows {
		meta := m.Status
		if m.SizeBytes > 0 {
			meta = fmt.Sprintf("%.1f GB · %s", float64(m.SizeBytes)/1e9, meta)
		}
		items = append(items, pickerItem{ID: m.CatalogID, Label: m.CatalogID, Meta: meta})
	}
	return pickFromList(prompt, items, seed)
}

func modelLs(asJSON bool) {
	cfg := loadConfigOrExit()
	st := openStoreOrExit(cfg)
	defer st.Close()
	ms, err := st.Models().List(context.Background())
	if err != nil {
		die("list models: %v", err)
	}
	if asJSON {
		if ms == nil {
			ms = []store.Model{}
		}
		emitJSON(ms)
		return
	}
	if len(ms) == 0 {
		fmt.Println(dim("(no models installed — try `opod model add llama-3.2-3b`)"))
		return
	}
	fmt.Printf("%s %s %s %s\n",
		bold(fmt.Sprintf("%-22s", "ID")),
		bold(fmt.Sprintf("%-10s", "STATUS")),
		bold(fmt.Sprintf("%-30s", "SOURCE")),
		bold("INSTALLED"))
	for _, m := range ms {
		fmt.Printf("%s %s %-30s %s\n",
			padCyan(m.CatalogID, 22),
			padStatus(m.Status, 10),
			m.Source,
			dim(m.InstalledAt.Format(time.RFC3339)))
	}
}

// padStatus colors the status word by tier: green=ready/ok, yellow=pulling/pending,
// red=error/failed. Pads to width `n` outside the color escape so columns align.
func padStatus(s string, n int) string {
	colored := s
	switch strings.ToLower(s) {
	case "ready", "ok", "running", "active":
		colored = green(s)
	case "pulling", "pending", "starting", "downloading":
		colored = yellow(s)
	case "error", "failed", "down", "draining":
		colored = red(s)
	}
	if len(s) >= n {
		return colored
	}
	return colored + strings.Repeat(" ", n-len(s))
}

func modelRemove(id string) {
	cfg := loadConfigOrExit()
	st := openStoreOrExit(cfg)
	defer st.Close()
	m, err := st.Models().Get(context.Background(), id)
	if err != nil {
		die("get model: %v", err)
	}
	if m == nil {
		die("no such model: %s", id)
	}
	eng := newEngineFromConfig(cfg)
	// Source format is "<engine_name>:<model_name>" — strip the first prefix.
	engineName := id
	if idx := strings.Index(m.Source, ":"); idx >= 0 && idx < len(m.Source)-1 {
		engineName = m.Source[idx+1:]
	}
	if err := eng.Delete(context.Background(), engineName); err != nil {
		warn(os.Stdout, "engine delete failed (continuing): %v", err)
	}
	if err := st.Models().Delete(context.Background(), id); err != nil {
		die("store delete: %v", err)
	}
	// Forget any desired placement so the next `opod up` doesn't try to
	// restore a model whose weights are gone.
	_ = st.DesiredPlacements().Delete(context.Background(), "local", id)
	ok(os.Stdout, "removed: %s", id)
}
