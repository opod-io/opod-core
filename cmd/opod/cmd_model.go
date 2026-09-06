package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/models"
	"github.com/opod-io/opod/internal/store"
	"gopkg.in/yaml.v3"
)

// truncStr is also defined in cmd_usage.go (this file uses it).
var _ = truncStr

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

// modelUnload asks the engine to drop a loaded model from RAM without
// deleting its weights from disk. When the leader is running, the admin
// endpoint is preferred — it drains in-flight requests first and clears
// the model's desired-placement row so it stays unloaded across
// restarts. With no leader (engine-only host), falls back to talking to
// the engine directly. Engines that don't support unload (vLLM, MLX-LM,
// llama-server) print a soft warning rather than failing.
func modelUnload(id string) {
	cfg := loadConfigOrExit()
	resp, adminErr := adminCall(context.Background(), cfg, "POST", "/admin/v1/models/"+id+"/unload", nil)
	if adminErr == nil {
		var out struct {
			Status string `json:"status"`
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(resp, &out)
		if out.Status == "noop" {
			warn(os.Stdout, "%s — restart the engine to free RAM", out.Reason)
			return
		}
		ok(os.Stdout, "unloaded %s (drained, weights still on disk; stays unloaded across restarts)", id)
		return
	}
	// A non-empty body means the leader IS running and refused — surface
	// its error instead of side-stepping it via the engine.
	if len(resp) > 0 {
		die("unload %s: %v: %s", id, adminErr, strings.TrimSpace(string(resp)))
	}
	// Leader not reachable — drive the engine directly.
	cat, err := models.LoadCatalog(cfg.CatalogDir)
	if err != nil {
		die("load catalog: %v", err)
	}
	entry := models.FindByID(cat, id)
	eng := newEngineFromConfig(cfg)
	name := id
	if entry != nil {
		if n := engineNativeName(eng.Name(), entry); n != "" {
			name = n
		}
	}
	// Bounded context: a wedged-but-listening engine should fail fast,
	// not hang the CLI indefinitely. 10s is enough for Ollama to
	// acknowledge keep_alive=0 even on a busy GPU.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := eng.Health(ctx); err != nil {
		die("engine not reachable (%v) — nothing to unload", err)
	}
	if err := eng.Unload(ctx, name); err != nil {
		if errors.Is(err, engines.ErrUnloadNotSupported) {
			warn(os.Stdout, "%s does not support online unload — restart the engine to free RAM", eng.Name())
			return
		}
		die("unload failed: %v", err)
	}
	ok(os.Stdout, "unloaded %s from %s (weights still on disk)", id, eng.Name())
}

// modelLoad asks the running leader to bring a model into engine memory
// with admission control (POST /admin/v1/models/{id}/load). The leader
// owns the router and the eviction/drain machinery, so unlike `unload`
// this command requires `opod up` to be running.
func modelLoad(id string, args []string) {
	var pin, swap bool
	priority := 0
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--pin", "-pin":
			pin = true
		case "--swap", "-swap":
			swap = true
		case "--priority", "-priority":
			if i+1 >= len(args) {
				die("--priority requires a value (usage: opod model load <id> [--swap] [--pin] [--priority N])")
			}
			if n, err := strconv.Atoi(args[i+1]); err == nil {
				priority = n
			}
			i++
		default:
			if strings.HasPrefix(args[i], "--priority=") {
				if n, err := strconv.Atoi(strings.TrimPrefix(args[i], "--priority=")); err == nil {
					priority = n
				}
				continue
			}
			die("unknown flag: %s (usage: opod model load <id> [--swap] [--pin] [--priority N])", args[i])
		}
	}
	cfg := loadConfigOrExit()
	body, _ := json.Marshal(map[string]any{"swap": swap, "pin": pin, "priority": priority})
	resp, err := adminCallT(context.Background(), cfg, "POST", "/admin/v1/models/"+id+"/load", body, weightsOpTimeout)
	if err != nil {
		renderLoadRefusal(id, resp, err)
		return
	}
	var out struct {
		Plan struct {
			AlreadyResident bool `json:"already_resident"`
			Victims         []struct {
				CatalogID string `json:"catalog_id"`
				SizeBytes int64  `json:"size_bytes"`
			} `json:"victims"`
		} `json:"plan"`
	}
	_ = json.Unmarshal(resp, &out)
	switch {
	case out.Plan.AlreadyResident:
		ok(os.Stdout, "%s is already resident — desired placement updated (pin=%t)", id, pin)
	case len(out.Plan.Victims) > 0:
		for _, v := range out.Plan.Victims {
			note(os.Stdout, "evicted %s (freed %.1f GB)", v.CatalogID, float64(v.SizeBytes)/1e9)
		}
		ok(os.Stdout, "loaded %s%s", id, pinSuffix(pin))
	default:
		ok(os.Stdout, "loaded %s%s", id, pinSuffix(pin))
	}
}

func pinSuffix(pin bool) string {
	if pin {
		return " (pinned — exempt from eviction and idle TTL)"
	}
	return ""
}

// renderLoadRefusal turns the structured 409/422 admission errors into
// actionable CLI output instead of a raw HTTP status.
func renderLoadRefusal(id string, resp []byte, callErr error) {
	var body struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
		Plan struct {
			Victims []struct {
				CatalogID string `json:"catalog_id"`
				SizeBytes int64  `json:"size_bytes"`
				LastUsed  int64  `json:"last_used"`
			} `json:"victims"`
		} `json:"plan"`
	}
	if json.Unmarshal(resp, &body) != nil || body.Error.Type == "" {
		die("load %s: %v", id, callErr)
	}
	switch body.Error.Type {
	case "needs_swap":
		warn(os.Stdout, "%s", body.Error.Message)
		for _, v := range body.Plan.Victims {
			note(os.Stdout, "  would evict %s (%.1f GB)", v.CatalogID, float64(v.SizeBytes)/1e9)
		}
		die("rerun with `opod model load %s --swap` to accept the eviction(s)", id)
	case "blocked_by_pinned":
		die("%s\n  (see pinned models with `opod model ps`; unpin by reloading without --pin, or unload one)", body.Error.Message)
	case "impossible":
		die("%s", body.Error.Message)
	default:
		die("load %s: %s", id, body.Error.Message)
	}
}

// modelPs renders the live memory picture: which models occupy engine
// RAM right now, their sizes, pins, and the node's remaining budget.
func modelPs(asJSON bool) {
	cfg := loadConfigOrExit()
	resp, err := adminCall(context.Background(), cfg, "GET", "/admin/v1/memory", nil)
	if err != nil {
		die("memory status: %v", err)
	}
	if asJSON {
		fmt.Println(string(resp))
		return
	}
	var st struct {
		Supported     bool  `json:"supported"`
		Exclusive     bool  `json:"exclusive"`
		TotalRAMBytes int64 `json:"total_ram_bytes"`
		BudgetBytes   int64 `json:"budget_bytes"`
		ResidentBytes int64 `json:"resident_bytes"`
		FreeBytes     int64 `json:"free_bytes"`
		Resident      []struct {
			CatalogID string     `json:"catalog_id"`
			SizeBytes int64      `json:"size_bytes"`
			VRAMBytes int64      `json:"vram_bytes"`
			Pinned    bool       `json:"pinned"`
			Priority  int        `json:"priority"`
			LastUsed  *time.Time `json:"last_used"`
		} `json:"resident"`
	}
	if err := json.Unmarshal(resp, &st); err != nil {
		die("decode memory status: %v", err)
	}
	if !st.Supported {
		fmt.Println(dim("(engine cannot report residency — memory view requires Ollama)"))
		return
	}
	gb := func(b int64) string { return fmt.Sprintf("%.1f GB", float64(b)/1e9) }
	if len(st.Resident) == 0 {
		fmt.Println(dim("(no models resident in engine memory)"))
	} else {
		fmt.Printf("%s %s %s %s %s %s\n",
			bold(fmt.Sprintf("%-26s", "MODEL")),
			bold(fmt.Sprintf("%9s", "RAM")),
			bold(fmt.Sprintf("%9s", "VRAM")),
			bold(fmt.Sprintf("%-7s", "PINNED")),
			bold(fmt.Sprintf("%4s", "PRIO")),
			bold("LAST USED"))
		for _, m := range st.Resident {
			pinned := dim("—")
			if m.Pinned {
				pinned = green("pinned")
			}
			last := dim("never")
			if m.LastUsed != nil {
				last = dim(m.LastUsed.Format("2006-01-02 15:04"))
			}
			fmt.Printf("%s %9s %9s %-16s %4d %s\n",
				padCyan(m.CatalogID, 26), gb(m.SizeBytes), gb(m.VRAMBytes), pinned, m.Priority, last)
		}
	}
	fmt.Println()
	mode := ""
	if st.Exclusive {
		mode = " · " + yellow("exclusive mode")
	}
	fmt.Printf("%s %s resident of %s budget (%s total RAM) · %s free%s\n",
		bold("Memory:"), gb(st.ResidentBytes), gb(st.BudgetBytes), gb(st.TotalRAMBytes), gb(st.FreeBytes), mode)
	fmt.Println(dim("Tip: `opod model load <id> --swap` evicts least-recently-used models to make room. `--pin` protects a model."))
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

// parseModelAddArgs extracts the model id and the --force / --dry-run /
// --from flags from the args passed after "model add". Order doesn't
// matter. `--from <path>` installs from a user-supplied catalog YAML
// (skipped when empty); the positional id is optional in that case and
// is taken from the YAML's `id:` field.
func parseModelAddArgs(args []string) (id string, force bool, dryRun bool, fromPath string, nodes []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--force", "-force":
			force = true
			continue
		case "--dry-run", "-dry-run", "--dryrun":
			dryRun = true
			continue
		case "--from", "-from":
			if i+1 >= len(args) {
				die("--from requires a path to a catalog YAML (usage: opod model add --from ./my-model.yaml)")
			}
			fromPath = args[i+1]
			i++
			continue
		case "--node", "-node", "--nodes", "-nodes":
			if i+1 >= len(args) {
				die("--node requires a node id (usage: opod model add <id> --node a,b,c)")
			}
			nodes = append(nodes, splitCSV(args[i+1])...)
			i++
			continue
		}
		if strings.HasPrefix(a, "--from=") {
			fromPath = strings.TrimPrefix(a, "--from=")
			continue
		}
		if strings.HasPrefix(a, "--node=") {
			nodes = append(nodes, splitCSV(strings.TrimPrefix(a, "--node="))...)
			continue
		}
		if strings.HasPrefix(a, "--nodes=") {
			nodes = append(nodes, splitCSV(strings.TrimPrefix(a, "--nodes="))...)
			continue
		}
		if id == "" {
			id = a
		}
	}
	return id, force, dryRun, fromPath, nodes
}

// modelAddDryRun prints the plan for installing `id` without actually
// pulling weights. Useful for sanity-checking before a long download.
// Accepts both catalog ids and scheme-prefixed ids (hf:/ollama:/file:);
// the scheme path skips the catalog lookup since these models have no
// pre-known size, license, or hardware floor.
func modelAddDryRun(id string) {
	cfg := loadConfigOrExit()
	var entry *models.Entry
	if e, ok := models.ParseSchemeID(id); ok {
		entry = e
	} else {
		cat := loadCatalogOrExit(cfg)
		entry = models.FindByID(cat, id)
	}
	if entry == nil {
		die("no catalog entry for %q (try `opod model search`)", id)
	}
	modelAddDryRunEntry(cfg, entry)
}

// modelAddDryRunEntry renders the plan for an already-resolved entry.
// Split out so `model add --from x.yaml --dry-run` can plan against the
// parsed YAML without persisting anything first.
func modelAddDryRunEntry(cfg *config.Config, entry *models.Entry) {
	bold, dim, reset := ansiCodes()
	fmt.Printf("%sDry-run plan for %s%s%s\n", bold, entry.ID, reset, "")
	fmt.Printf("  %sName%s          %s\n", bold, reset, entry.DisplayName)
	if entry.SizeBytes > 0 {
		fmt.Printf("  %sDownload size%s %.1f GB\n", bold, reset, float64(entry.SizeBytes)/1e9)
	}
	if entry.Hardware.MinRAMGB > 0 {
		fmt.Printf("  %sMin RAM%s       %d GB\n", bold, reset, entry.Hardware.MinRAMGB)
	}
	if entry.Hardware.MinVRAMGB > 0 {
		fmt.Printf("  %sMin VRAM%s      %d GB\n", bold, reset, entry.Hardware.MinVRAMGB)
	}
	if entry.License != "" {
		fmt.Printf("  %sLicense%s       %s\n", bold, reset, entry.License)
	}
	if entry.Released != "" {
		fmt.Printf("  %sReleased%s      %s\n", bold, reset, entry.Released)
	}

	if entry.Sharding.Required {
		fmt.Printf("  %sEngine%s        llama.cpp (sharded across %d workers)\n", bold, reset, entry.Sharding.DefaultShards)
		fmt.Printf("  %sNext step%s     opod shard create %s\n", bold, reset, entry.ID)
	} else {
		eng := newEngineFromConfig(cfg)
		engineName := engineNativeName(eng.Name(), entry)
		fmt.Printf("  %sEngine%s        %s (would pull %q)\n", bold, reset, eng.Name(), engineName)
		if err := eng.Health(context.Background()); err != nil {
			fmt.Printf("  %sEngine status%s %s%s — engine is not reachable; start it before `opod model add`%s\n", bold, reset, dim, err, reset)
		} else {
			fmt.Printf("  %sEngine status%s %sreachable%s\n", bold, reset, dim, reset)
		}
		if entry.Hardware.MinRAMGB == 0 {
			fmt.Printf("  %sHardware%s      %sno floor known — install will skip the pre-flight check%s\n", bold, reset, dim, reset)
		} else if msg := checkHardwareForModel(entry); msg != "" {
			fmt.Printf("  %sHardware%s      %s%s%s — would refuse without --force\n", bold, reset, dim, msg, reset)
		} else {
			fmt.Printf("  %sHardware%s      %sOK%s\n", bold, reset, dim, reset)
		}
		// Upstream reachability — same probe the real install runs.
		probeCtx, probeCancel := context.WithTimeout(context.Background(), models.ProbeTimeout)
		verdict, reason := models.ProbeSource(probeCtx, nil, entry)
		probeCancel()
		switch verdict {
		case models.ProbeOK:
			fmt.Printf("  %sSource check%s  %supstream exists%s\n", bold, reset, dim, reset)
		case models.ProbeNotFound:
			fmt.Printf("  %sSource check%s  NOT FOUND — install would refuse (%s)\n", bold, reset, reason)
		case models.ProbeIndeterminate:
			fmt.Printf("  %sSource check%s  %scould not verify (%s)%s\n", bold, reset, dim, reason, reset)
		}
		// Rough ETA. Assume 50 MB/s sustained over LAN/HF — pessimistic
		// enough that it's a useful "is this worth starting now?" number.
		if entry.SizeBytes > 0 {
			mins := float64(entry.SizeBytes) / (50.0 * 1024 * 1024) / 60
			fmt.Printf("  %sETA%s           ~%.0f min at 50 MB/s\n", bold, reset, mins)
		}
	}
	fmt.Println()
	fmt.Printf("%sNo weights pulled.%s Run `opod model add %s` to proceed.\n", dim, reset, entry.ID)
}

// modelAddOnNodes pins a model to specific workers by asking the running leader
// to drive the placement (POST /admin/v1/models with a "nodes" list). The leader
// resolves the catalog entry and dials each worker's /v1/model/load; the worker's
// engine pulls + loads it and reports it on the next heartbeat. Unlike the
// default install, this does NOT pull on the leader.
func modelAddOnNodes(id string, nodes []string) {
	cfg := loadConfigOrExit()
	body, _ := json.Marshal(map[string]any{"id": id, "nodes": nodes})
	note(os.Stdout, "placing %s on %s (workers pull + load; this may take a few minutes)…", id, strings.Join(nodes, ", "))
	resp, err := adminCallT(context.Background(), cfg, "POST", "/admin/v1/models", body, weightsOpTimeout)
	if err != nil {
		die("%v: %s", err, string(resp))
	}
	ok(os.Stdout, "placed %s on %s", id, strings.Join(nodes, ", "))
}

func modelAdd(id string, force bool) {
	if id == "" {
		die("usage: opod model add <id> [--force]")
	}
	cat := loadCatalogOrExit(loadConfigOrExit())
	entry := models.FindByID(cat, id)
	if entry == nil {
		die("no catalog entry for %q (try `opod model search`)", id)
	}
	modelAddEntry(entry, force)
}

// modelAddEntry runs the install flow for an already-resolved entry —
// either a catalog row (from `models.FindByID`) or a synthetic one built
// by `models.ParseSchemeID` for hf:/ollama:/file: ids. Shared because the
// hardware check, engine pull, and store/placement upsert are identical;
// only the entry source differs.
//
// For scheme-prefixed (custom) entries, SizeBytes and Hardware are zero
// so the hardware-floor check naturally short-circuits — we warn instead
// of refusing.
func modelAddEntry(entry *models.Entry, force bool) {
	cfg := loadConfigOrExit()

	// Pre-install hardware check — refuse if this machine clearly can't
	// run the model. Cheap to compute and saves the user a long failing
	// pull. Sharded entries are exempt (sharding is how you fit a model
	// that doesn't fit on any single node). Custom entries (no known
	// hardware floor) skip the check with a soft warning.
	if !entry.Sharding.Required {
		if entry.Hardware.MinRAMGB > 0 {
			if msg := checkHardwareForModel(entry); msg != "" {
				if force {
					warn(os.Stdout, "%s — proceeding because --force was set", msg)
				} else {
					die("%s\n  (override with `opod model add %s --force` if you know what you're doing)", msg, entry.ID)
				}
			}
		} else {
			warn(os.Stdout, "no hardware floor known for %s — proceeding without check (use a catalog model for pre-flight checks)", entry.ID)
		}
	}

	// Pre-flight source probe — a typo'd `hf:owner/repo` or a renamed
	// Ollama tag should fail HERE with the upstream URL, not later at
	// engine launch (vLLM/MLX/llama-server Pulls are no-ops, so without
	// this the add would "succeed" and the failure would surface as a
	// mystery engine crash). 404 is certain → refuse; network trouble is
	// not → warn and proceed. OPOD_SKIP_SOURCE_CHECK=1 bypasses.
	{
		probeCtx, probeCancel := context.WithTimeout(context.Background(), models.ProbeTimeout)
		verdict, reason := models.ProbeSource(probeCtx, nil, entry)
		probeCancel()
		switch verdict {
		case models.ProbeNotFound:
			die("source for %s does not exist: %s", entry.ID, reason)
		case models.ProbeIndeterminate:
			warn(os.Stdout, "could not verify source for %s (%s) — proceeding anyway", entry.ID, reason)
		}
	}

	// Sharded model? Hand off to the shard orchestrator on the leader.
	if entry.Sharding.Required {
		note(os.Stdout, "%s requires sharding — delegating to `opod shard create`", entry.ID)
		shardCreate(entry.ID, 0, nil, 0, 0)
		return
	}

	st := openStoreOrExit(cfg)
	defer st.Close()
	eng := newEngineFromConfig(cfg)

	// Refuse early if the source scheme has no path through this engine
	// (e.g. `ollama:foo` while the configured engine is vllm). The engine
	// would otherwise fail mid-pull with a less-obvious error.
	if !sourceCompatibleWithEngine(entry.Source.Type, eng.Name()) {
		die("source type %q in %s is not compatible with engine %s — switch engines or use a different scheme prefix",
			entry.Source.Type, entry.ID, eng.Name())
	}

	// Pick the engine-native model name based on which engine we're using.
	engineName := engineNativeName(eng.Name(), entry)
	if engineName == "" {
		die("catalog entry %s has no source name compatible with engine %s", entry.ID, eng.Name())
	}

	if err := eng.Health(context.Background()); err != nil {
		die("engine not reachable (%v) — start it first", err)
	}
	note(os.Stdout, "pulling %s (%s via %s) ...", entry.ID, engineName, eng.Name())
	bar := newProgressBar("pulling " + entry.ID)
	err := eng.Pull(context.Background(), engineName, bar.update)
	bar.done()
	if err != nil {
		die("pull failed: %v", err)
	}
	if err := st.Models().Upsert(context.Background(), store.Model{
		ID:          entry.ID,
		CatalogID:   entry.ID,
		Source:      eng.Name() + ":" + engineName,
		Status:      "ready",
		SizeBytes:   entry.SizeBytes,
		InstalledAt: time.Now(),
	}); err != nil {
		warn(os.Stdout, "the weights for %s are installed, but recording the install in the local registry failed: %v", entry.ID, err)
		die("rerun `opod model add %s` once the store is healthy to register it", entry.ID)
	}
	// Mark this node's placement so the router can find it. On a worker
	// this row will be reconciled by the leader on the next heartbeat too.
	if err := st.Placements().Upsert(context.Background(), store.Placement{
		NodeID:   "local",
		ModelID:  engineName,
		Status:   "ready",
		LastSeen: time.Now(),
	}); err != nil {
		warn(os.Stdout, "the weights for %s are installed, but recording its placement (so the router can find it) failed: %v", entry.ID, err)
		die("rerun `opod model add %s` once the store is healthy to register it", entry.ID)
	}
	ok(os.Stdout, "installed: %s", entry.ID)
}

// modelAddFromYAML loads a user-supplied catalog YAML at `path`, copies
// it into `~/.opod/catalog/<id>.yaml` so it persists across runs and
// shows up in `opod model search` / `info`, then runs the standard
// install flow. The copy step is what makes `--from` more useful than
// the scheme prefixes — once installed, the model is indistinguishable
// from a curated catalog entry.
func modelAddFromYAML(path string, force, dryRun bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		die("read %s: %v", path, err)
	}
	var entry models.Entry
	if err := yaml.Unmarshal(data, &entry); err != nil {
		die("parse %s: %v", path, err)
	}
	if entry.ID == "" {
		die("%s: missing `id:` field — a catalog entry needs at least an id and source", path)
	}
	if entry.Source.Type == "" {
		die("%s: missing `source.type:` field — set to ollama, huggingface, or file", path)
	}

	cfg := loadConfigOrExit()

	// Dry run plans against the parsed YAML and stops BEFORE the
	// copy-to-user-catalog write below — "no weights pulled" must also
	// mean "nothing persisted".
	if dryRun {
		modelAddDryRunEntry(cfg, &entry)
		return
	}

	// Persist into ~/.opod/catalog/ so this entry is visible to future
	// `opod model search/info/ls` runs. We use UserCatalogDir() to honor
	// OPOD_CATALOG_DIR when set; otherwise default to ~/.opod/catalog.
	dest, err := persistUserCatalogEntry(cfg.CatalogDir, entry.ID, data)
	if err != nil {
		warn(os.Stdout, "could not persist %s into the user catalog (%v) — proceeding with one-shot install", entry.ID, err)
	} else if dest != "" {
		note(os.Stdout, "saved %s to %s — visible to `opod model search` / `info` next run", entry.ID, dest)
	}

	modelAddEntry(&entry, force)
}

// persistUserCatalogEntry writes the YAML bytes to the resolved user
// catalog dir (defaulting to ~/.opod/catalog when OPOD_CATALOG_DIR is
// unset). Returns the destination path. Returns ("", nil) when no
// suitable directory could be resolved — caller falls back to a
// one-shot install. Does not overwrite an existing entry with the same
// id; surfaces a clear error so the user can rename or remove first.
func persistUserCatalogEntry(configuredDir, id string, data []byte) (string, error) {
	dir := configuredDir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home: %w", err)
		}
		dir = filepath.Join(home, ".opod", "catalog")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dir, err)
	}
	dest := filepath.Join(dir, id+".yaml")
	if _, err := os.Stat(dest); err == nil {
		return "", fmt.Errorf("%s already exists — remove or rename it first", dest)
	}
	if err := os.WriteFile(dest, data, 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", dest, err)
	}
	return dest, nil
}

// sourceCompatibleWithEngine reports whether a catalog (or synthetic)
// source type can be served by the named engine. Used as a pre-flight
// guard before kicking off the pull, so the user gets a clear "switch
// engines" message instead of a cryptic engine-side 404.
//
// An empty source type (older catalog entries) is treated as compatible
// — the engine will decide.
func sourceCompatibleWithEngine(sourceType, engineName string) bool {
	switch sourceType {
	case "", "auto":
		return true
	case "ollama":
		return engineName == "ollama"
	case "huggingface":
		return engineName == "vllm" || engineName == "mlx" || engineName == "mlx-lm" ||
			engineName == "tt-openai" || engineName == "tenstorrent" || engineName == "tt" ||
			strings.HasPrefix(engineName, "llamacpp") || strings.HasPrefix(engineName, "llama-cpp")
	case "file":
		return engineName == "mlx" || engineName == "mlx-lm" ||
			strings.HasPrefix(engineName, "llamacpp") || strings.HasPrefix(engineName, "llama-cpp")
	}
	return true
}

// engineNativeName picks the right field from the catalog source for a given
// engine (e.g. Source.OllamaName for ollama, Source.Repo for vllm/mlx).
// Falls back to e.ID when no engine-native field is set — callers that need
// to detect "no native name available" should compare against e.ID, not
// against "". (The earlier contract said it returned "" on a miss; the
// implementation never did, so the docstring was the bug.)
// engineNativeName delegates to models.EngineNativeName — the shared resolver
// the worker agent also uses, so leader and worker pick names identically.
func engineNativeName(engine string, e *models.Entry) string {
	return models.EngineNativeName(engine, e)
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

func modelSearch(query string, sortReleased bool, since string, asJSON bool) {
	cfg := loadConfigOrExit()
	cat := loadCatalogOrExit(cfg)
	q := strings.ToLower(query)

	// Find which models are already installed so we can mark them ✓
	installed := map[string]bool{}
	if st, err := store.OpenSQLite(cfg.Storage.DSN); err == nil {
		defer st.Close()
		if ms, err := st.Models().List(context.Background()); err == nil {
			for _, m := range ms {
				installed[m.CatalogID] = true
			}
		}
	}

	rows := make([]models.Entry, 0, len(cat))
	for _, e := range cat {
		if q != "" &&
			!strings.Contains(strings.ToLower(e.ID), q) &&
			!strings.Contains(strings.ToLower(e.DisplayName), q) &&
			!containsAny(e.Capabilities, q) &&
			!containsAny(e.Tags, q) {
			continue
		}
		if since != "" && (e.Released == "" || e.Released < since) {
			continue
		}
		rows = append(rows, e)
	}
	if sortReleased {
		sort.SliceStable(rows, func(i, j int) bool {
			// Newest first; entries without a date sink to the bottom.
			a, b := rows[i].Released, rows[j].Released
			if a == "" && b == "" {
				return rows[i].SizeBytes < rows[j].SizeBytes
			}
			if a == "" {
				return false
			}
			if b == "" {
				return true
			}
			return a > b
		})
	}

	if asJSON {
		// Decorate each entry with an `installed` field so scripts don't
		// have to cross-reference a separate query.
		type out struct {
			models.Entry
			Installed bool `json:"installed"`
		}
		decorated := make([]out, len(rows))
		for i, e := range rows {
			decorated[i] = out{Entry: e, Installed: installed[e.ID]}
		}
		emitJSON(decorated)
		return
	}

	fmt.Printf("%s %s %s %s %s %s %s\n",
		bold(fmt.Sprintf("%-26s", "ID")),
		bold(fmt.Sprintf("%-32s", "NAME")),
		bold(fmt.Sprintf("%7s", "SIZE")),
		bold(fmt.Sprintf("%5s", "RAM")),
		bold(fmt.Sprintf("%-22s", "CAPABILITIES")),
		bold(fmt.Sprintf("%-10s", "RELEASED")),
		bold("INSTALLED"))
	for _, e := range rows {
		size := "?"
		if e.SizeBytes > 0 {
			size = fmt.Sprintf("%.1f GB", float64(e.SizeBytes)/1e9)
		}
		caps := strings.Join(e.Capabilities, ",")
		if len(caps) > 22 {
			caps = caps[:21] + "…"
		}
		mark := ""
		if installed[e.ID] {
			mark = green("✓")
		}
		released := e.Released
		if released == "" {
			released = "—"
		}
		fmt.Printf("%s %-32s %7s %4dG %-22s %s %s\n",
			padCyan(e.ID, 26),
			truncStr(e.DisplayName, 32), size, e.Hardware.MinRAMGB, caps,
			padDim(released, 10),
			mark)
	}
	fmt.Println()
	fmt.Println(dim("Tip: `opod model info <id>` for full details on one model. `opod model add <id>` to install."))
}

// padCyan returns the colored string followed by trailing spaces so the
// visual column width is `n`. Using fmt's "%-26s" directly with a
// colored string would count the ANSI escape bytes as visible width.
func padCyan(s string, n int) string {
	if len(s) >= n {
		return cyan(s)
	}
	return cyan(s) + strings.Repeat(" ", n-len(s))
}

func padDim(s string, n int) string {
	if len(s) >= n {
		return dim(s)
	}
	return dim(s) + strings.Repeat(" ", n-len(s))
}

// parseModelSearchArgs extracts the query plus `--sort=released` and
// `--since YYYY-MM-DD` flags. Flags and the free-form query may appear
// in any order.
func parseModelSearchArgs(args []string) (query string, sortReleased bool, since string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--sort=released" || a == "--sort-released":
			sortReleased = true
		case a == "--sort":
			if i+1 < len(args) && args[i+1] == "released" {
				sortReleased = true
				i++
			}
		case strings.HasPrefix(a, "--since="):
			since = strings.TrimPrefix(a, "--since=")
		case a == "--since":
			if i+1 < len(args) {
				since = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "-"):
			die("unknown flag: %s (run `opod model --help`)", a)
		default:
			if query == "" {
				query = a
			}
		}
	}
	return
}

func containsAny(items []string, q string) bool {
	for _, it := range items {
		if strings.Contains(strings.ToLower(it), q) {
			return true
		}
	}
	return false
}

// modelInfo prints the full metadata for a single catalog model + whether
// it's installed locally + ready-to-paste usage snippets. Matches the kind
// of info in MODELS.md but condensed for terminal display.
func modelInfo(id string, asJSON bool) {
	cfg := loadConfigOrExit()
	cat := loadCatalogOrExit(cfg)
	entry := models.FindByID(cat, id)
	if entry == nil {
		die("no catalog entry for %q (try `opod model search`)", id)
	}
	if asJSON {
		emitJSON(entry)
		return
	}

	// Check installed state
	st, _ := store.OpenSQLite(cfg.Storage.DSN)
	var installedRow *store.Model
	if st != nil {
		defer st.Close()
		installedRow, _ = st.Models().Get(context.Background(), id)
	}

	bold, dim, reset := ansiCodes()

	fmt.Printf("%s%s%s — %s\n", bold, entry.ID, reset, entry.DisplayName)
	fmt.Println()

	// Status
	status := dim + "not installed" + reset
	if installedRow != nil {
		status = "installed · " + installedRow.Status + dim + " (" + installedRow.InstalledAt.Format("2006-01-02") + ")" + reset
	}
	fmt.Printf("  %sStatus%s         %s\n", bold, reset, status)

	// Source
	source := "—"
	switch entry.Source.Type {
	case "ollama":
		source = "ollama: " + entry.Source.OllamaName
	case "huggingface":
		source = "huggingface: " + entry.Source.Repo
		if entry.Source.File != "" {
			source += " (" + entry.Source.File + ")"
		}
	case "file":
		source = "file: " + entry.Source.Path
	}
	fmt.Printf("  %sSource%s         %s\n", bold, reset, source)

	// Size
	if entry.SizeBytes > 0 {
		fmt.Printf("  %sSize%s           %.1f GB\n", bold, reset, float64(entry.SizeBytes)/1e9)
	}
	if entry.Quant != "" {
		fmt.Printf("  %sQuant%s          %s\n", bold, reset, entry.Quant)
	}
	if entry.ContextWindow > 0 {
		fmt.Printf("  %sContext window%s %d tokens\n", bold, reset, entry.ContextWindow)
	}
	fmt.Printf("  %sMin RAM%s        %d GB\n", bold, reset, entry.Hardware.MinRAMGB)
	if entry.Hardware.MinVRAMGB > 0 {
		fmt.Printf("  %sMin VRAM%s       %d GB\n", bold, reset, entry.Hardware.MinVRAMGB)
	}
	if len(entry.Capabilities) > 0 {
		fmt.Printf("  %sCapabilities%s   %s\n", bold, reset, strings.Join(entry.Capabilities, ", "))
	}
	if len(entry.RecommendedEngines) > 0 {
		fmt.Printf("  %sEngines%s        %s\n", bold, reset, strings.Join(entry.RecommendedEngines, ", "))
	}
	if len(entry.Tags) > 0 {
		fmt.Printf("  %sTags%s           %s\n", bold, reset, strings.Join(entry.Tags, ", "))
	}
	if entry.Sharding.Required {
		fmt.Printf("  %sSharding%s       required (default %d shards, %s engine)\n",
			bold, reset, entry.Sharding.DefaultShards, entry.Sharding.Engine)
	}
	if entry.License != "" {
		licenseLine := entry.License
		if entry.LicenseURL != "" {
			licenseLine += " · " + dim + entry.LicenseURL + reset
		}
		fmt.Printf("  %sLicense%s        %s\n", bold, reset, licenseLine)
	}
	if entry.Released != "" {
		fmt.Printf("  %sReleased%s       %s\n", bold, reset, entry.Released)
	}
	if len(entry.Fallback) > 0 {
		fmt.Printf("  %sFallback%s       %s\n",
			bold, reset, strings.Join(entry.Fallback, " → "))
	}
	if len(entry.FallbackOnContextLength) > 0 {
		fmt.Printf("  %sFallback (context-length)%s  %s\n",
			bold, reset, strings.Join(entry.FallbackOnContextLength, " → "))
	}
	if len(entry.FallbackOnContentPolicy) > 0 {
		fmt.Printf("  %sFallback (content-policy)%s  %s\n",
			bold, reset, strings.Join(entry.FallbackOnContentPolicy, " → "))
	}

	// Pricing — vendor lookup first, then the catalog override if set.
	pp, pc := models.PriceFor(entry.ID, []models.Entry{*entry})
	if pp > 0 || pc > 0 {
		// Render as $/M tokens so the line reads like the vendor's
		// posted rate ("$3.00 / 1M prompt").
		fmt.Printf("  %sPricing%s        $%.2f / 1M prompt · $%.2f / 1M completion\n",
			bold, reset, pp*1000, pc*1000)
	} else {
		fmt.Printf("  %sPricing%s        %sfree (no cost tracking — open weights on your hardware)%s\n",
			bold, reset, dim, reset)
	}

	// Install + usage snippets — shape depends on what this model does.
	hasCap := func(c string) bool {
		for _, x := range entry.Capabilities {
			if x == c {
				return true
			}
		}
		return false
	}
	isEmbedding := hasCap("embedding")
	isVision := hasCap("vision")

	fmt.Println()
	fmt.Printf("%sInstall%s\n", bold, reset)
	if entry.Sharding.Required {
		fmt.Printf("  opod shard create %s %d\n", entry.ID, max2(entry.Sharding.DefaultShards))
	} else {
		fmt.Printf("  opod model add %s\n", entry.ID)
	}

	fmt.Println()
	if isEmbedding {
		fmt.Printf("%sUse via API (OpenAI embeddings shape)%s\n", bold, reset)
		fmt.Printf("  curl http://localhost:8080/v1/embeddings \\\n")
		fmt.Printf("    -H 'Authorization: Bearer sk-orc-...' \\\n")
		fmt.Printf("    -d '{\"model\":\"%s\",\"input\":\"hello world\"}'\n", entry.ID)
		fmt.Println()
		fmt.Printf("%sDrop-in for OpenAI text-embedding-* in any RAG library.%s\n", dim, reset)
	} else {
		fmt.Printf("%sUse via API (OpenAI shape)%s\n", bold, reset)
		fmt.Printf("  curl http://localhost:8080/v1/chat/completions \\\n")
		fmt.Printf("    -H 'Authorization: Bearer sk-orc-...' \\\n")
		fmt.Printf("    -d '{\"model\":\"%s\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}'\n", entry.ID)
		if isVision {
			fmt.Println()
			fmt.Printf("%sFor image input, send a content array with an `image_url` block:%s\n", dim, reset)
			fmt.Printf("  -d '{\"model\":\"%s\",\"messages\":[{\"role\":\"user\",\n", entry.ID)
			fmt.Printf("       \"content\":[{\"type\":\"text\",\"text\":\"what is this?\"},\n")
			fmt.Printf("                   {\"type\":\"image_url\",\"image_url\":{\"url\":\"data:image/png;base64,...\"}}]}]}'\n")
		}
		fmt.Println()
		fmt.Printf("%sUse via Claude Code%s\n", bold, reset)
		fmt.Printf("  export ANTHROPIC_BASE_URL=http://localhost:8080\n")
		fmt.Printf("  export ANTHROPIC_AUTH_TOKEN=sk-orc-...\n")
		fmt.Printf("  export ANTHROPIC_MODEL=%s\n", entry.ID)
		fmt.Printf("  claude\n")
	}
	fmt.Println()
	fmt.Printf("%sFull walkthrough%s   https://github.com/opod-io/opod/blob/main/MODELS.md#%s\n",
		bold, reset, strings.ReplaceAll(entry.ID, ".", "-"))
}

func max2(n int) int {
	if n < 2 {
		return 2
	}
	return n
}
