package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// cmdShard dispatches `opod shard <subcommand>`.
func cmdShard(args []string) {
	help := helpSpec{
		name:    "shard",
		summary: "orchestrate sharded models (one model split across N machines)",
		usage:   "opod shard <ls | create <model> [N] [--nodes a,b,c] [--tp N] [--pp N] | remove <model>>",
		examples: []string{
			"opod shard create llama-3.3-70b-sharded 2          # split across 2 auto-picked workers",
			"opod shard create llama-3.3-70b-sharded --nodes gpu-a,gpu-b  # pin to specific machines",
			"opod shard create llama-3.3-70b-sharded --nodes Titan  # N=1: whole model on one machine, no split",
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

func shardCreate(model string, n int, nodes []string, tp, pp int) {
	cfg := loadConfigOrExit()
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
