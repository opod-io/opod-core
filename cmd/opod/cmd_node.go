package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/opod-io/opod/internal/store"
)

// cmdNode dispatches `opod node <subcommand>`. ls/show/remove read and write
// the leader's store directly; drain/undrain go through the admin API when a
// leader is running (setNodeState).
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
			"opod node remove n_abc123            # forget it (prompts; worker keeps running)",
			"opod node remove n_abc123 --yes      # skip the prompt (for scripts)",
		},
		notes: []string{
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
	for _, n := range nodes {
		fmt.Printf("%-14s %-20s %-12s %-22s %-10s %s\n",
			n.ID, n.Hostname, n.OS+"/"+n.Arch, n.Address, n.State,
			n.LastHeartbeat.Format(time.RFC3339))
	}
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
	setNodeState(id, "drain", store.NodeStateDraining)
	ok(os.Stdout, "%s is draining: it gets no new requests and no new shard parts; requests already on it finish. A sharded model with a part on it stops serving.", id)
	note(os.Stdout, "it stays drained across heartbeats and a worker restart — undo with `opod node undrain %s`", id)
}

func nodeUndrain(id string) {
	setNodeState(id, "undrain", store.NodeStateReady)
	ok(os.Stdout, "%s is back in rotation: the router may choose it from the next request on", id)
}

// setNodeState asks the running leader (POST /admin/v1/nodes/{id}/<verb>, so
// the change is on its event stream); with no leader answering it writes the
// state column of the row directly — the leader reads it on every pick.
func setNodeState(id, verb, state string) {
	cfg := loadConfigOrExit()
	resp, adminErr := adminCall(context.Background(), cfg, "POST", "/admin/v1/nodes/"+url.PathEscape(id)+"/"+verb, nil)
	if adminErr == nil {
		return
	}
	// A body means the leader IS running and refused: its word is final.
	if len(resp) > 0 {
		die("%s %s: %v: %s", verb, id, adminErr, strings.TrimSpace(string(resp)))
	}
	st := openStoreOrExit(cfg)
	defer st.Close()
	found, err := st.Nodes().SetState(context.Background(), id, state)
	if err != nil {
		die("update node: %v", err)
	}
	if !found {
		die("no such node: %s", id)
	}
}

func nodeRemove(id string) {
	cfg := loadConfigOrExit()
	st := openStoreOrExit(cfg)
	defer st.Close()
	if err := st.Nodes().Delete(context.Background(), id); err != nil {
		die("delete node: %v", err)
	}
	ok(os.Stdout, "removed %s", id)
}
