package main

// `opod model add` — flags, the pre-flight policy (hardware floor, source
// probe, sharding hand-off) and printing; the pull + registration is
// models.Install.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/models"
	"gopkg.in/yaml.v3"
)

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
		engineName := models.EngineNativeName(eng.Name(), entry)
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
	if !models.SourceCompatibleWithEngine(entry.Source.Type, eng.Name()) {
		die("source type %q in %s is not compatible with engine %s — switch engines or use a different scheme prefix",
			entry.Source.Type, entry.ID, eng.Name())
	}
	note(os.Stdout, "pulling %s (%s via %s) ...", entry.ID, models.EngineNativeName(eng.Name(), entry), eng.Name())
	bar := newProgressBar("pulling " + entry.ID)
	_, err := models.Install(context.Background(), st, eng, entry, bar.update)
	bar.done()
	var regErr *models.RegisterError
	switch {
	case err == nil:
		ok(os.Stdout, "installed: %s", entry.ID)
	case errors.Is(err, models.ErrNoNativeName):
		die("catalog entry %s has no source name compatible with engine %s", entry.ID, eng.Name())
	case errors.As(err, &regErr):
		warn(os.Stdout, "the weights for %s are installed, but recording the %s failed: %v", entry.ID, regErr.What, regErr.Err)
		die("rerun `opod model add %s` once the store is healthy to register it", entry.ID)
	default:
		die("%v", err)
	}
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
	dest, err := models.PersistUserCatalogEntry(cfg.CatalogDir, entry.ID, data)
	if err != nil {
		warn(os.Stdout, "could not persist %s into the user catalog (%v) — proceeding with one-shot install", entry.ID, err)
	} else if dest != "" {
		note(os.Stdout, "saved %s to %s — visible to `opod model search` / `info` next run", entry.ID, dest)
	}

	modelAddEntry(&entry, force)
}
