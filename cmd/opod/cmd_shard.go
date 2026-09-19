package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/models"
	"github.com/opod-io/opod/internal/scheduler"
)

// cmdShard dispatches `opod shard <subcommand>`.
func cmdShard(args []string) {
	help := helpSpec{
		name:    "shard",
		summary: "orchestrate sharded models (one model split across N machines)",
		usage:   "opod shard <ls | create <model> [N] [--nodes a,b,c] [--tp N] [--pp N] | remove <model>>",
		examples: []string{
			"opod shard create llama-3.3-70b-sharded            # no count: picked from the live workers' free memory",
			"opod shard create llama-3.3-70b-sharded 2          # split across 2 auto-picked workers",
			"opod shard create llama-3.3-70b-sharded --nodes gpu-a,gpu-b  # pin to specific machines",
			"opod shard create llama-3.3-70b-sharded --nodes node-a  # N=1: whole model on one machine, no split",
			"opod shard create mimo-7b-ray --nodes gpu-a,gpu-b --tp 2  # vLLM TENSOR-parallel across 2 machines",
			"opod shard create mimo-7b-ray --nodes gpu-a,gpu-b --pp 2  # vLLM PIPELINE-parallel (the default)",
			"opod shard ls",
			"opod shard remove llama-3.3-70b-sharded            # prompts before tearing down",
			"opod shard remove llama-3.3-70b-sharded --yes      # skip the prompt (for scripts)",
		},
		notes: []string{
			"Sharding uses llama.cpp's RPC backend. Every shard worker needs `rpc-server` on PATH.",
			"The coordinator runs `llama-server` — by default on the highest-RAM worker (the leader only when there are no workers), so that machine needs `llama-server` on PATH. Override with OPOD_COORDINATOR_NODE=<node_id|local>.",
			"The catalog entry must have `sharding.required: true` and a local GGUF path in `source.path`.",
			"A shard count of 1 (or a single --nodes machine) runs the whole model on that host — no rpc-servers, just llama-server on the selected node (the coordinator override is ignored).",
			"With no count, --nodes, --tp or --pp, this command picks the shape itself: the smallest number of equal parts that fits the live workers' free memory (1 when the model fits one worker; never more parts than workers or than the model's layers) and prints what it picked and why. Any shape you name is sent untouched, and the admin API never picks — a body without a count means the catalog's default_shards.",
		},
	}
	if len(args) == 0 {
		dieHelp(help)
	}
	if wantsHelp(args) {
		showHelp(help)
	}
	switch args[0] {
	case "ls", "list":
		shardLs()
	case "create":
		rest, nodes := extractNodesFlag(args[1:])
		rest, tp := extractIntFlag(rest, "tp")
		rest, pp := extractIntFlag(rest, "pp")
		if len(rest) < 1 {
			die("usage: opod shard create <model> [shards] [--nodes a,b,c]")
		}
		n := 0
		if len(rest) >= 2 {
			parsed, err := strconv.Atoi(rest[1])
			if err != nil || parsed < 1 {
				die("invalid shard count %q (must be ≥1; 1 = whole model on one host)", rest[1])
			}
			n = parsed
		}
		shardCreate(rest[0], n, nodes, tp, pp)
	case "remove", "rm":
		rest, yes := extractYesFlag(args[1:])
		if len(rest) < 1 {
			die("usage: opod shard remove <model> [--yes]")
		}
		if !yes && !confirm(fmt.Sprintf("Tear down sharded model %q? The coordinator + every rpc-server will be stopped. (y/N) ", rest[0])) {
			die("aborted")
		}
		shardRemove(rest[0])
	default:
		dieUnknownSubcommand("shard", args[0], []string{"ls", "create", "remove"})
	}
}

func shardLs() {
	cfg := loadConfigOrExit()
	body, err := adminCall(context.Background(), cfg, "GET", "/admin/v1/shards", nil)
	if err != nil {
		die("%v: %s", err, string(body))
	}
	var shards []map[string]any
	_ = json.Unmarshal(body, &shards)
	if len(shards) == 0 {
		fmt.Println("(no shards — create one with `opod shard create <model>`)")
		return
	}
	fmt.Printf("%-32s %-14s %-12s %-22s %-10s\n", "MODEL", "ROLE", "NODE", "ADDRESS", "STATUS")
	for _, s := range shards {
		fmt.Printf("%-32s %-14s %-12s %-22s %-10s\n",
			fmt.Sprint(s["ModelID"]),
			fmt.Sprint(s["Role"]),
			fmt.Sprint(s["NodeID"]),
			fmt.Sprint(s["Address"]),
			fmt.Sprint(s["Status"]))
	}
}

// extractIntFlag pulls an optional `--<name> N` (or `--<name>=N`) out of args.
// Used for --tp/--pp, which mean something only to the vLLM sharding backend —
// llama.cpp's RPC split is layer-wise and has no tensor dimension.
func extractIntFlag(args []string, name string) (rest []string, val int) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--"+name || a == "-"+name:
			if i+1 < len(args) {
				val, _ = strconv.Atoi(args[i+1])
				i++
			}
		case strings.HasPrefix(a, "--"+name+"="):
			val, _ = strconv.Atoi(strings.TrimPrefix(a, "--"+name+"="))
		default:
			rest = append(rest, a)
		}
	}
	return rest, val
}

// extractNodesFlag pulls an optional `--nodes a,b,c` (comma-separated) out of
// args, returning the remaining positional args and the parsed node list.
func extractNodesFlag(args []string) (rest []string, nodes []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--nodes" || a == "-nodes":
			if i+1 < len(args) {
				nodes = splitCSV(args[i+1])
				i++
			}
		case strings.HasPrefix(a, "--nodes="):
			nodes = splitCSV(strings.TrimPrefix(a, "--nodes="))
		default:
			rest = append(rest, a)
		}
	}
	return rest, nodes
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// shardShape turns what the operator typed into what is sent. A named shape —
// a count, a node list, a tp/pp split — goes through untouched and pick is
// never consulted; only a bare `shard create <model>` asks the picker, and its
// answer is sent as an explicit node list so the leader builds exactly what
// was printed.
func shardShape(n int, nodes []string, tp, pp int, pick func() (scheduler.ShardPick, error)) (int, []string, string, error) {
	if n != 0 || len(nodes) > 0 || tp != 0 || pp != 0 {
		return n, nodes, "", nil
	}
	p, err := pick()
	if err != nil {
		return 0, nil, "", err
	}
	return p.Shards, p.Nodes, p.Why, nil
}

// pickShardsFromStore sizes the model against the leader's own rows — the
// workers it has heard from, what they registered and what is placed on them
// (scheduler.WorkerMemoryFacts). A model whose size the catalog does not
// record keeps the catalog's default_shards.
func pickShardsFromStore(cfg *config.Config, model string) (scheduler.ShardPick, error) {
	cat := loadCatalogOrExit(cfg)
	entry := models.FindByID(cat, model)
	if entry == nil {
		return scheduler.ShardPick{}, fmt.Errorf("no catalog entry for %q", model)
	}
	need := scheduler.ShardNeedBytes(*entry)
	if need == 0 {
		return scheduler.ShardPick{Why: "the catalog records no size for this model, so its default_shards applies"}, nil
	}
	st := openStoreOrExit(cfg)
	defer st.Close()
	maxAge := 60 * time.Second // the leader's own bound when the router check is off
	if s := cfg.Router.HeartbeatMaxAgeSeconds; s > 0 {
		maxAge = time.Duration(s) * time.Second
	}
	workers, err := scheduler.WorkerMemoryFacts(context.Background(), st, cat, model,
		cfg.Placement.ReservePercent, maxAge, time.Now())
	if err != nil {
		return scheduler.ShardPick{}, fmt.Errorf("read worker memory: %w", err)
	}
	return scheduler.PickShards(need, entry.Architecture.Layers, workers)
}

func shardCreate(model string, n int, nodes []string, tp, pp int) {
	cfg := loadConfigOrExit()
	n, nodes, why, err := shardShape(n, nodes, tp, pp, func() (scheduler.ShardPick, error) { return pickShardsFromStore(cfg, model) })
	if err != nil {
		die("cannot pick a shard count for %s: %v", model, err)
	}
	if why != "" {
		if n > 0 {
			note(os.Stdout, "picked %d part(s): %s", n, why)
		} else {
			note(os.Stdout, "%s", why)
		}
	}
	body, _ := json.Marshal(map[string]any{
		"model_id": model, "shards": n, "nodes": nodes, "tp": tp, "pp": pp,
	})
	if tp > 1 {
		note(os.Stdout, "tensor-parallel across machines (tp=%d): all-reduce twice per layer over the "+
			"network — expect it to be latency-bound", tp)
	}
	if len(nodes) > 0 {
		note(os.Stdout, "creating sharded model %s on %v (this may take a few minutes)…", model, nodes)
	} else {
		note(os.Stdout, "creating sharded model %s (this may take a few minutes)…", model)
	}
	resp, err := adminCallT(context.Background(), cfg, "POST", "/admin/v1/shards/create", body, weightsOpTimeout)
	if err != nil {
		die("%v: %s", err, string(resp))
	}
	var out map[string]string
	_ = json.Unmarshal(resp, &out)
	ok(os.Stdout, "sharded model ready: %s", out["model_id"])
}

func shardRemove(model string) {
	cfg := loadConfigOrExit()
	note(os.Stdout, "removing sharded model %s…", model)
	resp, err := adminCall(context.Background(), cfg, "DELETE", "/admin/v1/shards/"+url.PathEscape(model), nil)
	if err != nil {
		die("%v: %s", err, string(resp))
	}
	ok(os.Stdout, "removed %s", model)
}
