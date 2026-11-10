package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/opod-io/opod-core/internal/config"
	"github.com/opod-io/opod-core/internal/models"
	"github.com/opod-io/opod-core/internal/scheduler"
)

// cmdShard dispatches `opod shard <subcommand>`.
func cmdShard(args []string) {
	help := helpSpec{
		name:    "shard",
		summary: "orchestrate sharded models (one model split across N machines)",
		usage:   "opod shard <ls [--json] | create <model> [N] [--nodes a,b,c] [--tp N] [--pp N] [--gang ID] | remove <model> [--gang ID] [--yes]>",
		flagLines: []string{
			"N                         create: how many workers (default: picked from their free memory)",
			"--nodes a,b               create: the workers to use, in order (default: picked)",
			"--tp N / --pp N           create: tensor / pipeline parallel degree for a vLLM gang (default: pipeline)",
			"--gang ID                 create: a named gang, served beside the others; remove: only that gang",
			"--yes / -y                remove: skip the prompt",
		},
		examples: []string{
			"opod shard create llama-3.3-70b-sharded            # no count: picked from the live workers' free memory",
			"opod shard create llama-3.3-70b-sharded 2          # split across 2 auto-picked workers",
			"opod shard create llama-3.3-70b-sharded --nodes gpu-a,gpu-b  # pin to specific machines",
			"opod shard create llama-3.3-70b-sharded --nodes node-a  # N=1: whole model on one machine, no split",
			"opod shard create mimo-7b-ray --nodes gpu-a,gpu-b --tp 2  # vLLM TENSOR-parallel across 2 machines",
			"opod shard create mimo-7b-ray --nodes gpu-a,gpu-b --pp 2  # vLLM PIPELINE-parallel (the default)",
			"opod shard create llama-3.3-70b-sharded --gang g1 --nodes gpu-c,gpu-d  # a SECOND gang: another copy that serves in parallel",
			"opod shard ls",
			"opod shard remove llama-3.3-70b-sharded            # prompts before tearing down",
			"opod shard remove llama-3.3-70b-sharded --yes      # skip the prompt (for scripts)",
			"opod shard remove llama-3.3-70b-sharded --gang g1  # one gang; the others keep serving",
		},
		notes: []string{
			"Sharding uses llama.cpp's RPC backend. Every shard worker needs `rpc-server` on PATH.",
			"The coordinator runs `llama-server` — by default on the worker with the biggest card, and a worker with no card is chosen only when no candidate has one (then by host RAM; the leader only when there are no workers), so that machine needs `llama-server` on PATH. Override with OPOD_COORDINATOR_NODE=<node_id|local>.",
			"The catalog entry must have `sharding.required: true` and a local GGUF path in `source.path`.",
			"A shard count of 1 (or a single --nodes machine) runs the whole model on that host — no rpc-servers, just llama-server on the selected node (the coordinator override is ignored).",
			"A model may have SEVERAL gangs — independent copies of the weights, each serving whole requests, which the leader load-balances across. `--gang <id>` names one: creating it leaves the model's other gangs serving, and removing it leaves the rest. Without --gang, create replaces every gang of the model and remove takes them all down.",
			"Without --nodes, the parts go to the strongest workers: a worker with a card is always preferred to one without, then the biggest card wins, then the biggest host. Name --nodes to place them yourself, in the order you name.",
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
		rest, asJSON := extractJSONFlag(args[1:])
		if len(rest) > 0 {
			dieUsageOf("shard", "shard ls takes only --json (got %q)", rest[0])
		}
		shardLs(asJSON)
	case "create":
		rest, nodes := extractNodesFlag(args[1:])
		rest, tp := extractIntFlag(rest, "tp")
		rest, pp := extractIntFlag(rest, "pp")
		rest, gang := extractStrFlag(rest, "gang")
		for _, a := range rest {
			if strings.HasPrefix(a, "-") {
				dieUsageOf("shard", "unknown shard create flag %s (have: --nodes, --tp, --pp, --gang)", a)
			}
		}
		if len(rest) < 1 {
			dieUsageOf("shard", "shard create is missing an argument: opod shard create <model> [shards] [--nodes a,b,c] [--tp N] [--pp N] [--gang ID]")
		}
		if len(rest) > 2 {
			dieUsageOf("shard", "shard create takes a model and a count (got %q)", strings.Join(rest, " "))
		}
		n := 0
		if len(rest) == 2 {
			parsed, err := strconv.Atoi(rest[1])
			if err != nil || parsed < 1 {
				dieUsageOf("shard", "shard count %q must be a whole number ≥ 1 (1 = the whole model on one host)", rest[1])
			}
			n = parsed
		}
		shardCreate(rest[0], gang, n, nodes, tp, pp)
	case "remove", "rm":
		rest, yes := extractYesFlag(args[1:])
		rest, gang := extractStrFlag(rest, "gang")
		if len(rest) < 1 {
			dieUsageOf("shard", "shard remove is missing an argument: opod shard remove <model> [--gang ID] [--yes]")
		}
		what := fmt.Sprintf("Tear down sharded model %q? The coordinator + every rpc-server will be stopped.", rest[0])
		if gang != "" {
			what = fmt.Sprintf("Tear down gang %q of %q? Its coordinator + rpc-servers stop; the model's other gangs keep serving.", gang, rest[0])
		}
		if !yes {
			confirmOrExit(what+" (y/N) ", "shard remove "+rest[0])
		}
		shardRemove(rest[0], gang)
	default:
		dieUnknownSubcommand("shard", args[0], []string{"ls", "create", "remove"})
	}
}

// shardRows is the leader's shard list; a body that is not one is an error,
// never "no shards".
func shardRows(cfg *config.Config) ([]map[string]any, []byte) {
	body, err := adminCall(context.Background(), cfg, "GET", "/admin/v1/shards", nil)
	if err != nil {
		die("list shards: %v", err)
	}
	var shards []map[string]any
	if err := json.Unmarshal(body, &shards); err != nil {
		die("list shards: the leader's answer is not a shard list (%v) — is %s an opod leader?", err, adminTargetURL(cfg))
	}
	return shards, body
}

// cell prints a row's field, "-" when the leader did not send it.
func cell(v any) string {
	if v == nil || fmt.Sprint(v) == "" {
		return "-"
	}
	return fmt.Sprint(v)
}

func shardLs(asJSON bool) {
	cfg := loadConfigOrExit()
	shards, body := shardRows(cfg)
	if asJSON {
		fmt.Println(strings.TrimSpace(string(body)))
		return
	}
	if len(shards) == 0 {
		fmt.Println("(no shards — create one with `opod shard create <model>`)")
		return
	}
	fmt.Printf("%-32s %-6s %-14s %-12s %-22s %-10s\n", "MODEL", "GANG", "ROLE", "NODE", "ADDRESS", "STATUS")
	for _, s := range shards {
		fmt.Printf("%-32s %-6s %-14s %-12s %-22s %-10s\n",
			cell(s["ModelID"]),
			gangOrDefault(s["GangID"]),
			cell(s["Role"]),
			cell(s["NodeID"]),
			cell(s["Address"]),
			cell(s["Status"]))
	}
}

// extractIntFlag pulls an optional `--<name> N` (or `--<name>=N`) out of args.
// Used for --tp/--pp, which mean something only to the vLLM sharding backend —
// llama.cpp's RPC split is layer-wise and has no tensor dimension.
func extractIntFlag(args []string, name string) (rest []string, val int) {
	num := func(v string) int {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			// Read as 0 it meant "no split" and the command went on (T10.21).
			dieUsageOf("shard", "--%s %q is not a whole number", name, v)
		}
		return n
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--"+name || a == "-"+name:
			if i+1 >= len(args) {
				dieUsageOf("shard", "--%s needs a number", name)
			}
			val = num(args[i+1])
			i++
		case strings.HasPrefix(a, "--"+name+"="):
			val = num(strings.TrimPrefix(a, "--"+name+"="))
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
		case a == "--nodes" || a == "-nodes" || a == "--node" || a == "-node":
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				dieUsageOf("shard", "%s needs node ids (a,b,c; ids: opod node ls)", a)
			}
			nodes = splitCSV(args[i+1])
			i++
		case strings.HasPrefix(a, "--nodes=") || strings.HasPrefix(a, "--node="):
			nodes = splitCSV(a[strings.Index(a, "=")+1:])
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
	// 0 (the router's check is off) means scheduler.DefaultHeartbeatMaxAge —
	// the same bound, by the same rule (scheduler.WorkerFor), the leader applies.
	maxAge := time.Duration(cfg.Router.HeartbeatMaxAgeSeconds) * time.Second
	workers, err := scheduler.WorkerMemoryFacts(context.Background(), st, cat, model,
		cfg.Placement.ReservePercent, maxAge, time.Now())
	if err != nil {
		return scheduler.ShardPick{}, fmt.Errorf("read worker memory: %w", err)
	}
	return scheduler.PickShards(need, entry.Architecture.Layers, workers)
}

func shardCreate(model, gang string, n int, nodes []string, tp, pp int) {
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
	checkShardNodes(cfg, nodes)
	body, _ := json.Marshal(map[string]any{
		"model_id": model, "shards": n, "nodes": nodes, "tp": tp, "pp": pp, "gang": gang,
	})
	if tp > 1 {
		note(os.Stdout, "tensor-parallel across machines (tp=%d): all-reduce twice per layer over the "+
			"network — expect it to be latency-bound", tp)
	}
	if len(nodes) > 0 {
		note(os.Stdout, "creating sharded model %s on %s (this may take a few minutes; Ctrl-C stops waiting and may leave a partial gang)…", model, strings.Join(nodes, ", "))
	} else {
		note(os.Stdout, "creating sharded model %s (this may take a few minutes; Ctrl-C stops waiting and may leave a partial gang)…", model)
	}
	// A create the CLI stops waiting for can leave a partial gang on the
	// leader (rpc parts with no coordinator): say so, and how to clean up.
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	cleanup := "opod shard remove " + model
	if gang != "" {
		cleanup += " --gang " + gang
	}
	stopTicking := tickWhile("creating " + model)
	_, err = adminCallT(ctx, cfg, "POST", "/admin/v1/shards/create", body, weightsOpTimeout)
	stopTicking()
	if ctx.Err() != nil {
		warn("interrupted — the leader may hold a partial gang of %s; clean it up with: %s", model, cleanup)
		os.Exit(130)
	}
	if err != nil {
		die("create sharded model %s: %v\n  a failed create can leave a partial gang; check opod shard ls, clean up with: %s", model, err, cleanup)
	}
	ok(os.Stdout, "%s ready: %s — see opod shard ls", model, describeGang(cfg, model, gang))
}

// describeGang names what the create built, from the leader's own rows: the
// gang, its nodes and its head (the coordinator).
func describeGang(cfg *config.Config, model, gang string) string {
	shards, _ := shardRows(cfg)
	var nodes []string
	head, id := "", gang
	for _, s := range shards {
		if cell(s["ModelID"]) != model || (gang != "" && cell(s["GangID"]) != gang) {
			continue
		}
		if id == "" {
			id = gangOrDefault(s["GangID"])
		}
		nodes = append(nodes, cell(s["NodeID"]))
		if strings.Contains(cell(s["Role"]), "coord") {
			head = cell(s["NodeID"])
		}
	}
	if len(nodes) == 0 {
		return "the leader lists no part of it yet"
	}
	d := fmt.Sprintf("gang %s on %s", id, strings.Join(nodes, ", "))
	if head != "" {
		d += " (head " + head + ")"
	}
	return d
}

// checkShardNodes refuses --nodes names the leader does not know, before the
// create: the leader would refuse them after minutes of setup, or not at all.
func checkShardNodes(cfg *config.Config, nodes []string) {
	if len(nodes) == 0 {
		return
	}
	raw, err := adminCall(context.Background(), cfg, "GET", "/admin/v1/nodes", nil)
	if err != nil {
		return // the create below says what is wrong with the leader
	}
	var rows []struct {
		ID string `json:"ID"`
	}
	if json.Unmarshal(raw, &rows) != nil {
		return
	}
	known := map[string]bool{}
	for _, r := range rows {
		known[r.ID] = true
	}
	for _, n := range nodes {
		if !known[n] {
			dieUsageOf("shard", "the leader has no node %q (ids: opod node ls)", n)
		}
	}
}

func shardRemove(model, gang string) {
	cfg := loadConfigOrExit()
	path := "/admin/v1/shards/" + url.PathEscape(model)
	what := model
	if gang != "" {
		path += "/" + url.PathEscape(gang)
		what = model + " gang " + gang
	}
	found := false
	shards, _ := shardRows(cfg)
	for _, s := range shards {
		if cell(s["ModelID"]) == model && (gang == "" || cell(s["GangID"]) == gang) {
			found = true
		}
	}
	if !found {
		ok(os.Stdout, "no gang of %s to remove — nothing changed", what)
		return
	}
	note(os.Stdout, "removing %s…", what)
	if _, err := adminCall(context.Background(), cfg, "DELETE", path, nil); err != nil {
		die("remove %s: %v", what, err)
	}
	ok(os.Stdout, "removed %s", what)
}

// gangOrDefault prints a gang id, defaulting a row that carries none.
func gangOrDefault(v any) string {
	if s, _ := v.(string); s != "" {
		return s
	}
	return "g0"
}

// extractStrFlag pulls an optional `--<name> V` (or `--<name>=V`) out of args.
func extractStrFlag(args []string, name string) (rest []string, val string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--"+name:
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				dieUsageOf("shard", "--%s needs a value", name)
			}
			val = args[i+1]
			i++
		case strings.HasPrefix(a, "--"+name+"="):
			val = strings.TrimPrefix(a, "--"+name+"=")
		default:
			rest = append(rest, a)
		}
	}
	return rest, val
}
