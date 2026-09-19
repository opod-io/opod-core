package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/store"
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
		examples: []string{
			"opod node ls",
			"opod node show n_abc123",
			"opod node drain n_abc123             # no new requests or shard parts go to it; in-flight finishes",
			"opod node undrain n_abc123           # back in rotation",
			"opod node remove n_abc123            # forget it, through the running leader (prompts; worker keeps running)",
			"opod node remove n_abc123 --yes      # skip the prompt (for scripts)",
		},
		notes: []string{
			"`ls` shows the state the leader acts on: `lost` when a worker's heartbeats stopped, `draining` when you drained it.",
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
		nodeLs()
	case "show":
		if len(args) < 2 {
			die("usage: opod node show <id>")
		}
		nodeShow(args[1])
	case "drain":
		if len(args) < 2 {
			die("usage: opod node drain <id>")
		}
		nodeDrain(args[1])
	case "undrain":
		if len(args) < 2 {
			die("usage: opod node undrain <id>")
		}
		nodeUndrain(args[1])
	case "remove", "rm":
		rest, yes := extractYesFlag(args[1:])
		if len(rest) < 1 {
			die("usage: opod node remove <id> [--yes]")
		}
		if !yes && !confirm(fmt.Sprintf("Remove node %q from the cluster? The worker keeps running but the leader will forget it. (y/N) ", rest[0])) {
			die("aborted")
		}
		nodeRemove(rest[0])
	default:
		dieUnknownSubcommand("node", args[0], []string{"ls", "show", "drain", "undrain", "remove"})
	}
}

func nodeLs() {
	cfg := loadConfigOrExit()
	st := openStoreOrExit(cfg)
	defer st.Close()
	nodes, err := st.Nodes().List(context.Background())
	if err != nil {
		die("list nodes: %v", err)
	}
	if len(nodes) == 0 {
		fmt.Println("(no nodes registered yet — issue a join token: `opod token create --node`)")
		return
	}
	fmt.Printf("%-14s %-20s %-12s %-22s %-10s %s\n", "ID", "HOSTNAME", "OS/ARCH", "ADDRESS", "STATE", "LAST HB")
	for _, r := range nodeRows(cfg, nodes, time.Now()) {
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
func nodeRows(cfg *config.Config, nodes []store.Node, now time.Time) []nodeRow {
	maxAge := store.HeartbeatBound(cfg.Router.HeartbeatMaxAgeSeconds)
	out := make([]nodeRow, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, nodeRow{
			ID: n.ID, Hostname: n.Hostname, Platform: n.OS + "/" + n.Arch, Address: n.Address,
			State: n.LiveState(maxAge, now), LastHB: n.LastHeartbeat.Format(time.RFC3339),
		})
	}
	return out
}

func nodeShow(id string) {
	cfg := loadConfigOrExit()
	st := openStoreOrExit(cfg)
	defer st.Close()
	n, err := st.Nodes().Get(context.Background(), id)
	if err != nil {
		die("get node: %v", err)
	}
	if n == nil {
		die("no such node: %s", id)
	}
	// Include the models currently resident on this node, from the heartbeat-
	// reconciled placements — so callers (and any UI over this API) can see what's
	// actually loaded on the worker, not just its hardware/state.
	loaded := []string{}
	if ps, e := st.Placements().GetByNode(context.Background(), id); e == nil {
		for _, p := range ps {
			loaded = append(loaded, p.ModelID)
		}
	}
	out := struct {
		*store.Node
		LoadedModels []string `json:"loadedModels"`
	}{n, loaded}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
}

// nodeDrain takes a worker out of rotation; nodeUndrain puts it back.
func nodeDrain(id string) {
	if id == "local" {
		die("the leader's own node cannot be drained — unload its models (`opod model unload <id>`) or stop the leader (`opod down`)")
	}
	cfg := loadConfigOrExit()
	via, err := setNodeState(cfg, id, "drain", store.NodeStateDraining)
	if err != nil {
		die("drain %s: %v", id, err)
	}
	ok(os.Stdout, "%s is draining (%s): it gets no new requests and no new shard parts; requests already on it finish. A sharded model with a part on it stops serving.", id, via)
	note(os.Stdout, "it stays drained across heartbeats and a worker restart — undo with `opod node undrain %s`", id)
}

func nodeUndrain(id string) {
	cfg := loadConfigOrExit()
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
// leader that answers with a refusal is final (its body is the error); only
// a call that reached nobody falls back to the store.
func throughLeader(cfg *config.Config, method, path string) (took bool, err error) {
	resp, adminErr := adminCall(context.Background(), cfg, method, path, nil)
	if adminErr == nil {
		return true, nil
	}
	if len(resp) > 0 {
		return true, fmt.Errorf("%v: %s", adminErr, strings.TrimSpace(string(resp)))
	}
	return false, nil
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
