package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/opod-io/opod/internal/config"
)

// cmdRoute manages the cross-provider routing chain — the ordered list of
// model ids Opod walks for model="auto" (and as fallback). Same data the
// dashboard's Routing tab drag-reorders; this is the source of truth.
//
//	opod route ls
//	opod route set groq/llama-3.3-70b,gemini/gemini-2.0-flash,qwen3.6-27b
//	opod route add deepseek/deepseek-chat --after gemini/gemini-2.0-flash
//	opod route mv qwen3.6-27b --bottom
//	opod route rm claude-3-5-haiku-latest
//	opod route reset
func cmdRoute(args []string) {
	help := helpSpec{
		name:    "route",
		summary: "manage the cross-provider routing chain (model=\"auto\" order)",
		usage:   "opod route <ls | set a,b,c | add <id> [--top|--bottom|--after X|--before X] | mv <id> --top|--bottom|--after X|--before X | rm <id> | reset>",
		examples: []string{
			"opod route ls                                              # show the current order",
			"opod route set groq/llama-3.3-70b,gemini/gemini-2.0-flash,qwen3.6-27b",
			"opod route add deepseek/deepseek-chat                      # append at the bottom",
			"opod route add claude-3-5-haiku-latest --after gemini/gemini-2.0-flash",
			"opod route mv qwen3.6-27b --bottom                         # local model as the floor",
			"opod route rm claude-3-5-haiku-latest                      # drop an entry",
			"opod route reset                                           # back to the computed default",
		},
		notes: []string{
			"Opod tries entries top-to-bottom for model=\"auto\", skipping any that's rate-limited or unavailable.",
			"Vendor entries are prefixed (groq/…, gemini/…, deepseek/…); bare ids are local catalog models.",
			"Keep a local model at the bottom — it's never rate-limited, so it's the always-available floor.",
			"The dashboard Routing tab drag-reorders the same list; both write the same audited endpoint.",
		},
	}
	if len(args) == 0 {
		dieHelp(help)
	}
	if wantsHelp(args) {
		help.print(os.Stdout)
		return
	}
	switch args[0] {
	case "ls", "list":
		routeLs(args[1:])
	case "set":
		if len(args) < 2 {
			die("usage: opod route set a,b,c")
		}
		routeSet(splitChain(args[1]))
	case "add":
		routeAdd(args[1:])
	case "mv", "move":
		routeMove(args[1:])
	case "rm", "remove", "delete":
		if len(args) < 2 {
			die("usage: opod route rm <id>")
		}
		routeRemove(args[1])
	case "reset":
		routeReset()
	default:
		dieUnknownSubcommand("route", args[0], []string{"ls", "set", "add", "mv", "rm", "reset"})
	}
}

// splitChain parses a comma-separated chain into trimmed, non-empty ids.
func splitChain(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// routeFetch GETs the current chain (or the computed default) from the leader.
func routeFetch(cfg *config.Config) ([]string, bool) {
	body, err := adminCall(context.Background(), cfg, "GET", "/admin/v1/route", nil)
	if err != nil {
		die("%v: %s", err, string(body))
	}
	var resp struct {
		Chain     []string `json:"chain"`
		IsDefault bool     `json:"is_default"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		die("decode route: %v", err)
	}
	return resp.Chain, resp.IsDefault
}

// routePut PUTs a chain and prints it back.
func routePut(cfg *config.Config, chain []string) {
	payload, _ := json.Marshal(map[string][]string{"chain": chain})
	body, err := adminCall(context.Background(), cfg, "PUT", "/admin/v1/route", payload)
	if err != nil {
		die("%v: %s", err, string(body))
	}
	printChain(chain, false)
}

func routeLs(args []string) {
	_, asJSON := extractJSONFlag(args)
	cfg := loadConfigOrExit()
	chain, isDefault := routeFetch(cfg)
	if asJSON {
		emitJSON(map[string]any{"chain": chain, "is_default": isDefault})
		return
	}
	printChain(chain, isDefault)
}

func routeSet(chain []string) {
	cfg := loadConfigOrExit()
	routePut(cfg, chain)
}

func routeAdd(args []string) {
	if len(args) == 0 {
		die("usage: opod route add <id> [--top|--bottom|--after X|--before X]")
	}
	id := args[0]
	pos, anchor := parsePosition(args[1:])
	cfg := loadConfigOrExit()
	chain, _ := routeFetch(cfg)
	chain = remove(chain, id) // de-dupe if already present
	chain = insertAt(chain, id, pos, anchor)
	routePut(cfg, chain)
}

func routeMove(args []string) {
	if len(args) == 0 {
		die("usage: opod route mv <id> --top|--bottom|--after X|--before X")
	}
	id := args[0]
	pos, anchor := parsePosition(args[1:])
	if pos == posBottom && anchor == "" && len(args) == 1 {
		die("mv needs a position: --top|--bottom|--after X|--before X")
	}
	cfg := loadConfigOrExit()
	chain, _ := routeFetch(cfg)
	if indexOf(chain, id) < 0 {
		die("%q is not in the chain", id)
	}
	chain = remove(chain, id)
	chain = insertAt(chain, id, pos, anchor)
	routePut(cfg, chain)
}

func routeRemove(id string) {
	cfg := loadConfigOrExit()
	chain, _ := routeFetch(cfg)
	if indexOf(chain, id) < 0 {
		die("%q is not in the chain", id)
	}
	routePut(cfg, remove(chain, id))
}

func routeReset() {
	cfg := loadConfigOrExit()
	body, err := adminCall(context.Background(), cfg, "DELETE", "/admin/v1/route", nil)
	if err != nil {
		die("%v: %s", err, string(body))
	}
	ok(os.Stdout, "routing chain reset to the computed default")
	chain, isDefault := routeFetch(cfg)
	printChain(chain, isDefault)
}

// position flags for add/mv.
type position int

const (
	posBottom position = iota
	posTop
	posAfter
	posBefore
)

func parsePosition(args []string) (position, string) {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--top":
			return posTop, ""
		case "--bottom":
			return posBottom, ""
		case "--after":
			if i+1 < len(args) {
				return posAfter, args[i+1]
			}
			die("--after needs a model id")
		case "--before":
			if i+1 < len(args) {
				return posBefore, args[i+1]
			}
			die("--before needs a model id")
		}
	}
	return posBottom, ""
}

func insertAt(chain []string, id string, pos position, anchor string) []string {
	switch pos {
	case posTop:
		return append([]string{id}, chain...)
	case posAfter, posBefore:
		ai := indexOf(chain, anchor)
		if ai < 0 {
			die("anchor %q is not in the chain", anchor)
		}
		at := ai
		if pos == posAfter {
			at = ai + 1
		}
		out := make([]string, 0, len(chain)+1)
		out = append(out, chain[:at]...)
		out = append(out, id)
		out = append(out, chain[at:]...)
		return out
	default: // posBottom
		return append(chain, id)
	}
}

func indexOf(chain []string, id string) int {
	for i, m := range chain {
		if m == id {
			return i
		}
	}
	return -1
}

func remove(chain []string, id string) []string {
	out := make([]string, 0, len(chain))
	for _, m := range chain {
		if m != id {
			out = append(out, m)
		}
	}
	return out
}

func printChain(chain []string, isDefault bool) {
	if len(chain) == 0 {
		fmt.Println("(empty chain — set one with `opod route set a,b,c`)")
		return
	}
	label := ""
	if isDefault {
		label = "  (computed default — not yet saved)"
	}
	fmt.Printf("routing chain%s\n", label)
	for i, m := range chain {
		tag := ""
		if !strings.Contains(m, "/") {
			tag = "  [local]"
		}
		fmt.Printf("  %2d. %s%s\n", i+1, m, tag)
	}
	note(os.Stdout, "reorder with `opod route mv <id> --top|--after X` or set the whole list with `opod route set a,b,c`")
}
