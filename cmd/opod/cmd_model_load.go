package main

// `opod model load|unload|ps` — the leader's admission-controlled load path
// (/admin/v1/models/{id}/load, /unload, /memory) rendered for the terminal.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/models"
)

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
		if n := models.EngineNativeName(eng.Name(), entry); n != "" {
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
