package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/opod-io/opod-core/internal/config"
	"github.com/opod-io/opod-core/internal/store"
)

// cmdNode dispatches `opod node <subcommand>`. ls/show read the leader's
// store; drain/undrain/remove go through the admin API when a leader is
// running and fall back to the store when none answers (throughLeader), and
// say which it was.
func cmdNode(args []string) {
	help := helpSpec{
		name:    "node",
		summary: "manage worker nodes in the cluster",
		usage:   "opod node <ls | show <id> | drain <id> | undrain <id> | remove <id>>",
		flagLines: []string{
			"--yes / -y                remove: skip the prompt",
		},
		examples: []string{
			"opod node ls",
			"opod node show n_abc123",
			"opod node drain n_abc123             # no new requests or shard parts go to it; in-flight finishes",
			"opod node undrain n_abc123           # back in rotation",
			"opod node remove n_abc123            # forget it, through the running leader (prompts; worker keeps running)",
			"opod node remove n_abc123 --yes      # skip the prompt (for scripts)",
		},
		notes: []string{
			"`ls` shows the state the leader acts on: `lost` when a worker's heartbeats stopped, `engine-silent` when it heartbeats but its engine has not answered it for a minute, `draining` when you drained it.",
			"drain, undrain and remove ask the running leader and fall back to the store only when none answers; each says which.",
			"Add a new node: `opod token create --node` then on the worker run `opod join \"<url>?token=…\"` (quoted: `?` is a glob in zsh).",
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
			dieUsageOf("node", "node ls takes only --json (got %q)", rest[0])
		}
		nodeLs(asJSON)
	case "show":
		rest, asJSON := extractJSONFlag(args[1:])
		switch {
		case len(rest) == 0:
			dieUsageOf("node", "node show is missing an argument: opod node show <id> [--json]")
		case len(rest) > 1:
			dieUsageOf("node", "node show takes one node id (got %q)", strings.Join(rest, " "))
		}
		nodeShow(rest[0], asJSON)
	case "drain":
		if len(args) < 2 {
			dieUsageOf("node", "node drain is missing an argument: opod node drain <id>")
		}
		nodeDrain(args[1])
	case "undrain":
		if len(args) < 2 {
			dieUsageOf("node", "node undrain is missing an argument: opod node undrain <id>")
		}
		nodeUndrain(args[1])
	case "remove", "rm":
		rest, yes := extractYesFlag(args[1:])
		if len(rest) < 1 {
			dieUsageOf("node", "node remove is missing an argument: opod node remove <id> [--yes]")
		}
		if !yes {
			confirmOrExit(fmt.Sprintf("Remove node %q from the cluster? The worker keeps running but the leader will forget it. (y/N) ", rest[0]), "node remove "+rest[0])
		}
		nodeRemove(rest[0])
	default:
		dieUnknownSubcommand("node", args[0], []string{"ls", "show", "drain", "undrain", "remove"})
	}
}

func nodeLs(asJSON bool) {
	cfg := loadConfigOrExit()
	nodes, live, err := listNodes(cfg)
	if err != nil {
		die("list nodes: %v", err)
	}
	if asJSON {
		rows := nodeRows(cfg, nodes, live, time.Now())
		out := make([]map[string]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, map[string]string{"id": r.ID, "hostname": r.Hostname, "platform": r.Platform, "address": r.Address, "state": r.State, "last_heartbeat": r.LastHB})
		}
		emitJSON(out)
		return
	}
	if len(nodes) == 0 {
		fmt.Println("(no nodes registered yet — issue a join token: `opod token create --node`)")
		return
	}
	fmt.Printf("%-14s %-20s %-12s %-22s %-10s %s\n", "ID", "HOSTNAME", "OS/ARCH", "ADDRESS", "STATE", "LAST HB")
	for _, r := range nodeRows(cfg, nodes, live, time.Now()) {
		fmt.Printf("%-14s %-20s %-12s %-22s %-10s %s\n", r.ID, r.Hostname, r.Platform, r.Address, r.State, r.LastHB)
	}
}

// nodeRow is one line of `opod node ls`.
type nodeRow struct{ ID, Hostname, Platform, Address, State, LastHB string }

// nodeRows renders the listing. STATE is the state the leader acts on, not
// the stored column: the same rule the router routes by (store.Node.LiveState
// under the leader's heartbeat bound), so a worker whose heartbeats stopped
// reads `lost` here exactly when the leader stops sending it requests, and a
// drained one reads `draining`.
// live, when the running leader answered, is the state it reported per node id,
// which wins over a recomputation under this host's config.
func nodeRows(cfg *config.Config, nodes []store.Node, live map[string]string, now time.Time) []nodeRow {
	maxAge := store.HeartbeatBound(cfg.Router.HeartbeatMaxAgeSeconds)
	out := make([]nodeRow, 0, len(nodes))
	for _, n := range nodes {
		state := live[n.ID]
		if state == "" {
			state = n.LiveState(maxAge, now)
		}
		out = append(out, nodeRow{
			ID: n.ID, Hostname: n.Hostname, Platform: n.OS + "/" + n.Arch, Address: n.Address,
			State: state, LastHB: n.LastHeartbeat.Format(time.RFC3339),
		})
	}
	return out
}

// listNodes is the running leader's registry, or the store's when no leader
// answers on this host (live is then nil).
func listNodes(cfg *config.Config) (nodes []store.Node, live map[string]string, err error) {
	rows, took, err := nodesFromLeader(cfg)
	if err != nil {
		return nil, nil, err
	}
	if took {
		live = map[string]string{}
		for _, r := range rows {
			nodes = append(nodes, r.Node)
			live[r.ID] = r.Live
		}
		return nodes, live, nil
	}
	st := openStoreOrExit(cfg)
	defer st.Close()
	nodes, err = st.Nodes().List(context.Background())
	return nodes, nil, err
}

func nodeShow(id string, asJSON bool) {
	cfg := loadConfigOrExit()
	n, live, loaded, err := showNode(cfg, id)
	if err != nil {
		die("get node: %v", err)
	}
	if n == nil {
		die("no node %s (see opod node ls)", id)
	}
	if asJSON {
		fmt.Println(nodeShowJSON(*n, loaded))
		return
	}
	maxAge := store.HeartbeatBound(cfg.Router.HeartbeatMaxAgeSeconds)
	fmt.Printf("%s  %s\n", bold(n.ID), n.Hostname)
	if live == "" {
		live = n.LiveState(maxAge, time.Now())
	}
	fmt.Printf("  state      %s (last heartbeat %s)\n", live, n.LastHeartbeat.Format(time.RFC3339))
	fmt.Printf("  platform   %s/%s · %d GB RAM\n", n.OS, n.Arch, n.RAMGB)
	fmt.Printf("  address    %s\n", n.Address)
	if len(loaded) == 0 {
		fmt.Println("  loaded     (nothing)")
	} else {
		fmt.Printf("  loaded     %s\n", strings.Join(loaded, ", "))
	}
	fmt.Println(dim("  (opod node show " + n.ID + " --json for every field)"))
}

// nodeDrain takes a worker out of rotation; nodeUndrain puts it back.
func nodeDrain(id string) {
	if id == "local" {
		die("the leader's own node cannot be drained — unload its models (`opod model unload <id>`) or stop the leader (`opod down`)")
	}
	cfg := loadConfigOrExit()
	if storedNodeState(cfg, id) == store.NodeStateDraining {
		ok(os.Stdout, "%s is already draining — nothing changed (undo with `opod node undrain %s`)", id, id)
		return
	}
	via, err := setNodeState(cfg, id, "drain", store.NodeStateDraining)
	if err != nil {
		die("drain %s: %v", id, err)
	}
	ok(os.Stdout, "%s is draining (%s): it gets no new requests and no new shard parts; requests already on it finish. A sharded model with a part on it stops serving.", id, via)
	note(os.Stdout, "it stays drained across heartbeats and a worker restart — undo with `opod node undrain %s`", id)
}

func nodeUndrain(id string) {
	cfg := loadConfigOrExit()
	if s := storedNodeState(cfg, id); s != "" && s != store.NodeStateDraining {
		ok(os.Stdout, "%s is not drained — nothing changed", id)
		return
	}
	via, err := setNodeState(cfg, id, "undrain", store.NodeStateReady)
	if err != nil {
		die("undrain %s: %v", id, err)
	}
	ok(os.Stdout, "%s is back in rotation (%s): the router may choose it from the next request on", id, via)
}

// The two ways a node command reaches the leader's state, as the command
// reports them.
const (
	viaLeader = "through the running leader"
	viaStore  = "written to the store — no leader answered"
)

// throughLeader runs one admin call and says whether a leader took it. A
// leader that answers at all is final — a refusal, an error page, a 500 with
// no body — and only THIS host's leader not running falls back to the store:
// a store written under a leader that answered is the one thing not to do.
func throughLeader(cfg *config.Config, method, path string) (took bool, err error) {
	_, took, err = askLeader(cfg, method, path)
	return took, err
}

// askLeader is throughLeader with the leader's answer: the read verbs (node ls,
// node show) ask the running leader first too, because on a managed host the
// store this command could open is not the leader's (PLAN T24.102).
func askLeader(cfg *config.Config, method, path string) (body []byte, took bool, err error) {
	body, adminErr := adminCall(context.Background(), cfg, method, path, nil)
	switch {
	case adminErr == nil:
		return body, true, nil
	case errors.Is(adminErr, errNoAdminKey):
		// No key reached nobody — but a leader may still run (T10.21).
		if leaderAnswers(cfg) {
			return nil, true, fmt.Errorf("a leader answers at %s but this host has no admin key (%s, OPOD_TOKEN and OPOD_ADMIN_TOKEN are empty), so this cannot go through it; run this on the host where `opod up` wrote the key, or set OPOD_TOKEN", localLeaderURL(cfg), localAdminKeyPath(cfg))
		}
		return nil, false, nil
	case localLeaderDown(adminErr):
		return nil, false, nil
	}
	return nil, true, adminErr
}

// leaderNode is one row of the running leader's GET /admin/v1/nodes: the node,
// the state the leader acts on, and what it has resident.
type leaderNode struct {
	store.Node
	Live       string            `json:"state"`
	Placements []store.Placement `json:"placements,omitempty"`
}

// nodesFromLeader is the running leader's registry; took=false means no leader
// answered on this host, and only then may a caller read the store.
func nodesFromLeader(cfg *config.Config) (rows []leaderNode, took bool, err error) {
	body, took, err := askLeader(cfg, "GET", "/admin/v1/nodes")
	if !took || err != nil {
		return nil, took, err
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, true, fmt.Errorf("read the leader's node list: %w", err)
	}
	return rows, true, nil
}

// setNodeState asks the running leader (POST /admin/v1/nodes/{id}/<verb>, so
// the change is on its event stream); with no leader answering it writes the
// state column of the row directly — a leader reads it on every pick.
func setNodeState(cfg *config.Config, id, verb, state string) (via string, err error) {
	if took, err := throughLeader(cfg, "POST", "/admin/v1/nodes/"+url.PathEscape(id)+"/"+verb); took {
		return viaLeader, err
	}
	st := openStoreOrExit(cfg)
	defer st.Close()
	found, err := st.Nodes().SetState(context.Background(), id, state)
	if err != nil {
		return "", fmt.Errorf("update node: %w", err)
	}
	if !found {
		return "", fmt.Errorf("no such node")
	}
	return viaStore, nil
}

func nodeRemove(id string) {
	cfg := loadConfigOrExit()
	via, err := removeNode(cfg, id)
	if err != nil {
		die("remove %s: %v", id, err)
	}
	if via == viaLeader {
		ok(os.Stdout, "removed %s (%s): its placements are gone and the router dropped its cached connection, cooldown and sticky pins", id, via)
		return
	}
	ok(os.Stdout, "removed %s and its placements (%s)", id, via)
}

// showNode is one node and the models resident on it — from the running
// leader's registry, else (no leader answering on this host) the store's rows,
// the heartbeat-reconciled placements, so callers see what is actually loaded
// on the worker, not just its hardware. live is the leader's state ("" = none).
func showNode(cfg *config.Config, id string) (n *store.Node, live string, loaded []string, err error) {
	loaded = []string{}
	rows, took, err := nodesFromLeader(cfg)
	if err != nil {
		return nil, "", nil, err
	}
	if took {
		for _, r := range rows {
			if r.ID == id {
				for _, p := range r.Placements {
					loaded = append(loaded, p.ModelID)
				}
				node := r.Node
				return &node, r.Live, loaded, nil
			}
		}
		return nil, "", nil, nil
	}
	st := openStoreOrExit(cfg)
	defer st.Close()
	n, err = st.Nodes().Get(context.Background(), id)
	if err != nil || n == nil {
		return n, "", loaded, err
	}
	if ps, e := st.Placements().GetByNode(context.Background(), id); e == nil {
		for _, p := range ps {
			loaded = append(loaded, p.ModelID)
		}
	}
	return n, "", loaded, nil
}

// removeNode forgets a node. A running leader must do it itself (DELETE
// /admin/v1/nodes/{id}): besides the rows it holds a cached connection to the
// worker, its cooldown and its sticky pins in memory, and a row deleted
// behind its back leaves all of that — and the node's placements — in place.
// With no leader answering, the node row and its placements are deleted from
// the store.
func removeNode(cfg *config.Config, id string) (via string, err error) {
	if took, err := throughLeader(cfg, "DELETE", "/admin/v1/nodes/"+url.PathEscape(id)); took {
		return viaLeader, err
	}
	st := openStoreOrExit(cfg)
	defer st.Close()
	ctx := context.Background()
	n, err := st.Nodes().Get(ctx, id)
	if err != nil {
		return "", err
	}
	if n == nil {
		return "", fmt.Errorf("no such node")
	}
	if err := st.Placements().ReplaceForNode(ctx, id, nil); err != nil {
		return "", fmt.Errorf("delete placements: %w", err)
	}
	if err := st.Nodes().Delete(ctx, id); err != nil {
		return "", fmt.Errorf("delete node: %w", err)
	}
	return viaStore, nil
}

// nodeShowJSON is `opod node show`'s output: the node row with its worker
// token redacted (config.Redact, as /admin/v1/config does) — a terminal is
// no place for the credential the leader calls the worker back with — and
// the models resident on it.
func nodeShowJSON(n store.Node, loaded []string) string {
	n.WorkerToken = config.Redact(n.WorkerToken)
	out := struct {
		store.Node
		LoadedModels []string `json:"loadedModels"`
	}{n, loaded}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		die("node show: %v", err)
	}
	return string(b)
}

// storedNodeState is the node's stored state column — what drain and
// undrain set — as the running leader's registry reports it, else as this
// host's store holds it; "" when neither holds the row.
func storedNodeState(cfg *config.Config, id string) string {
	// The running leader's row first (T24.102): on a managed host the store
	// this command could open is not the leader's, and the guard refuses it.
	n, _, _, err := showNode(cfg, id)
	if err != nil || n == nil {
		return ""
	}
	return n.State
}
