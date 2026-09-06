package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/opod-io/opod/internal/control"
)

func cmdInvite(args []string) {
	fs := flag.NewFlagSet("invite", flag.ExitOnError)
	var (
		quota   = fs.Int64("quota", 0, "daily token quota for the new user (0 = unlimited)")
		clients = fs.String("clients", "", "comma-separated client IDs to include in the share card (default: all)")
		format  = fs.String("format", "markdown", "output format: markdown | json")
		baseURL = fs.String("base-url", "", "Opod base URL to embed in the share card (default: cfg.ExternalURL or http://localhost:<listen>)")
		model   = fs.String("model", "auto", "model id to suggest in the snippets")
	)
	help := helpSpec{
		name:    "invite",
		summary: "create a user-scope token and print a complete share card for a teammate",
		usage:   "opod invite <name> [--quota N] [--clients id1,id2,…] [--format markdown|json]",
		flags:   fs,
		examples: []string{
			"opod invite hadi",
			"opod invite alice --quota 100000",
			"opod invite bob --clients claude-code,cursor",
			"opod invite eve --format json   # for scripting",
		},
		notes: []string{
			"The token is shown exactly once — store or transmit it immediately.",
			"Revoke later with: opod token revoke <id> (id is shown in the output).",
			"This command must run on the leader host (it writes to the local store).",
		},
	}
	if wantsHelp(args) {
		showHelp(help)
	}
	args = reorderFlagsFirst(args, map[string]bool{
		"-quota": true, "--quota": true,
		"-clients": true, "--clients": true,
		"-format": true, "--format": true,
		"-base-url": true, "--base-url": true,
		"-model": true, "--model": true,
	})
	_ = fs.Parse(args)

	rest := fs.Args()
	if len(rest) == 0 {
		dieHelp(help)
	}
	if len(rest) > 1 {
		die("invite takes exactly one name (got: %s)", strings.Join(rest, " "))
	}
	name := rest[0]

	var clientList []string
	if *clients != "" {
		for _, c := range strings.Split(*clients, ",") {
			c = strings.TrimSpace(c)
			if c != "" {
				clientList = append(clientList, c)
			}
		}
	}

	cfg := loadConfigOrExit()
	resolvedURL := resolveBaseURL(cfg, *baseURL)
	st := openStoreOrExit(cfg)
	defer st.Close()

	res, err := control.Invite(context.Background(), st, control.InviteInput{
		Name:             name,
		BaseURL:          resolvedURL,
		QuotaDailyTokens: *quota,
		Clients:          clientList,
		Model:            *model,
	})
	if err != nil {
		die("%v", err)
	}

	switch strings.ToLower(*format) {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		// Marshal a lean payload — full ConnectOutput is verbose.
		payload := map[string]any{
			"name":          res.Record.Name,
			"token_id":      res.Record.ID,
			"token":         res.Token,
			"scope":         res.Record.Scope,
			"base_url":      res.BaseURL,
			"quota_daily":   res.Record.QuotaDailyTokens,
			"created_at":    res.Record.CreatedAt,
			"snippets":      snippetsAsMap(res),
			"clients_order": res.ClientsOrder,
		}
		_ = enc.Encode(payload)
	case "markdown", "":
		// Terminal-friendly: status lines, then the paste-into-Slack card.
		ok(os.Stdout, "created user '%s' (token id=%s, scope=user)", res.Record.Name, res.Record.ID)
		fmt.Println()
		fmt.Println("  Token (shown once — store or transmit it now):")
		fmt.Printf("    %s\n", res.Token)
		fmt.Println()
		fmt.Println("  Share card (paste-into-Slack markdown):")
		fmt.Println("  ─────────────────────────────────────────────────")
		fmt.Println()
		fmt.Println(res.MarkdownCard())
		fmt.Println("  ─────────────────────────────────────────────────")
	default:
		die("unknown format %q (valid: markdown, json)", *format)
	}
}

func snippetsAsMap(res *control.InviteResult) map[string]string {
	out := make(map[string]string, len(res.Snippets))
	for k, v := range res.Snippets {
		out[k] = v.Snippet
	}
	return out
}
