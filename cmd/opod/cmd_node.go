package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/opod-io/opod/internal/store"
)

// cmdNode dispatches `opod node <subcommand>`. All operations call the
// admin API on the leader (local by default).
func cmdNode(args []string) {
	help := helpSpec{
		name:    "node",
		summary: "manage worker nodes in the cluster",
		usage:   "opod node <ls | show <id> | drain <id> | remove <id>>",
		examples: []string{
			"opod node ls",
			"opod node show n_abc123",
			"opod node drain n_abc123             # stop routing new requests to it",
			"opod node remove n_abc123            # forget it (prompts; worker keeps running)",
			"opod node remove n_abc123 --yes      # skip the prompt (for scripts)",
		},
		notes: []string{
			"Add a new node: `opod token create --node` then on the worker run `opod join <url>?token=…`.",
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
		dieUnknownSubcommand("node", args[0], []string{"ls", "show", "drain", "remove"})
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

func nodeDrain(id string) {
	cfg := loadConfigOrExit()
	st := openStoreOrExit(cfg)
	defer st.Close()
	n, err := st.Nodes().Get(context.Background(), id)
	if err != nil {
		die("look up node %s: %v", id, err)
	}
	if n == nil {
		die("no such node: %s", id)
	}
	n.State = "draining"
	if err := st.Nodes().Upsert(context.Background(), *n); err != nil {
		die("update node: %v", err)
	}
	ok(os.Stdout, "marked %s as draining", id)
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
