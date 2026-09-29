package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/opod-io/opod/internal/auth"
)

func cmdToken(args []string) {
	help := helpSpec{
		name:    "token",
		summary: "manage API keys and node-join tokens",
		usage:   "opod token <create [name] [--admin|--node] [--ttl D | --expires-at DATE] | ls | expire <id> [--in D] | renew <id> --ttl D | revoke <id>>",
		examples: []string{
			"opod token create alice                            # user-scope key for dev `alice`",
			"opod token create alice-admin --admin              # admin-scope key (can call /admin/v1/*)",
			"opod token create alice --ttl 7d                   # auto-expire in 7 days",
			"opod token create alice --expires-at 2026-07-01    # absolute expiry date",
			"opod token expire k_abc                            # expire immediately",
			"opod token expire k_abc --in 1h                    # expire in 1 hour",
			"opod token renew  k_abc --ttl 30d                  # extend expiry by 30 days from now",
			"opod token create --node                           # one-time join token for a new worker",
			"opod token ls",
			"opod token revoke k_abc123",
		},
		notes: []string{
			"⚠️  --node tokens are the shared secret leader ↔ worker — only issue on a trusted network (LAN or Tailscale).",
			"A key is an identity — who is calling, with which scope, until when. Per-caller model allowlists, per-minute ceilings and daily quotas are the application layer's in front of an endpoint, not the runtime's (they left core 2026-09-28).",
			"`--ttl` accepts Go-style durations (`30s`, `5m`, `2h`) plus `d` for days (`7d`). `--expires-at` is YYYY-MM-DD or RFC3339.",
		},
	}
	if len(args) == 0 {
		dieHelp(help)
	}
	if wantsHelp(args) {
		showHelp(help)
	}
	switch args[0] {
	case "create":
		name := "default"
		scope := "user"
		var expiresAt time.Time
		for i := 1; i < len(args); i++ {
			a := args[i]
			switch a {
			case "--admin":
				scope = "admin"
			case "--node":
				scope = "node"
				if name == "default" {
					name = "node-join"
				}
			case "--ttl":
				if i+1 >= len(args) {
					die("--ttl requires a duration (e.g. 30m, 2h, 7d)")
				}
				d, err := parseFlexibleDuration(args[i+1])
				if err != nil {
					die("invalid --ttl: %v", err)
				}
				expiresAt = time.Now().Add(d)
				i++
			case "--expires-at":
				if i+1 >= len(args) {
					die("--expires-at requires YYYY-MM-DD or RFC3339")
				}
				t, err := parseFlexibleDate(args[i+1])
				if err != nil {
					die("invalid --expires-at: %v", err)
				}
				expiresAt = t
				i++
			default:
				if strings.HasPrefix(a, "--ttl=") {
					d, err := parseFlexibleDuration(strings.TrimPrefix(a, "--ttl="))
					if err != nil {
						die("invalid --ttl: %v", err)
					}
					expiresAt = time.Now().Add(d)
					continue
				}
				if strings.HasPrefix(a, "--expires-at=") {
					t, err := parseFlexibleDate(strings.TrimPrefix(a, "--expires-at="))
					if err != nil {
						die("invalid --expires-at: %v", err)
					}
					expiresAt = t
					continue
				}
				if name == "default" {
					name = a
				}
			}
		}
		tokenCreate(name, scope, expiresAt)
	case "expire":
		if len(args) < 2 {
			die("usage: opod token expire <id> [--in DURATION]")
		}
		tokenExpire(args[1], args[2:])
	case "renew":
		if len(args) < 2 {
			die("usage: opod token renew <id> --ttl DURATION | --expires-at DATE")
		}
		tokenRenew(args[1], args[2:])
	case "ls", "list":
		tokenList()
	case "revoke":
		if len(args) < 2 {
			die("usage: opod token revoke <id>")
		}
		tokenRevoke(args[1])
	default:
		dieUnknownSubcommand("token", args[0], []string{"create", "ls", "expire", "renew", "revoke"})
	}
}

func tokenCreate(name, scope string, expiresAt time.Time) {
	cfg := loadConfigOrExit()
	st := openStoreOrExit(cfg)
	defer st.Close()
	// The token's name doubles as its UserID today. Once OIDC lands the
	// UserID will come from the issuing admin's session.
	userID := name
	if scope == "node" {
		userID = "" // node tokens have no owner
	}
	plain, rec, err := auth.Generate(name, scope, userID)
	if err != nil {
		die("generate: %v", err)
	}
	rec.ExpiresAt = expiresAt
	if err := st.APIKeys().Create(context.Background(), rec); err != nil {
		die("persist key: %v", err)
	}
	ok(os.Stdout, "created %s (id=%s, scope=%s)", name, rec.ID, scope)
	if !expiresAt.IsZero() {
		fmt.Printf("  expires: %s (in %s)\n", expiresAt.Format(time.RFC3339), time.Until(expiresAt).Round(time.Second))
	}
	fmt.Println()
	fmt.Println("  Key (shown once — store it now):")
	emitSecret(plain)
}

func tokenList() {
	cfg := loadConfigOrExit()
	st := openStoreOrExit(cfg)
	defer st.Close()
	keys, err := st.APIKeys().List(context.Background())
	if err != nil {
		die("list keys: %v", err)
	}
	if len(keys) == 0 {
		fmt.Println("(no API keys — create one with `opod token create`)")
		return
	}
	fmt.Printf("%-14s %-20s %-8s %-7s %-20s %s\n", "ID", "NAME", "SCOPE", "REVOKED", "EXPIRES", "CREATED")
	for _, k := range keys {
		rev := "no"
		if k.Revoked {
			rev = "yes"
		}
		expires := "never"
		if !k.ExpiresAt.IsZero() {
			expires = k.ExpiresAt.Format(time.RFC3339)
		}
		fmt.Printf("%-14s %-20s %-8s %-7s %-20s %s\n",
			k.ID, k.Name, k.Scope, rev, expires, k.CreatedAt.Format(time.RFC3339))
	}
}

func tokenRevoke(id string) {
	cfg := loadConfigOrExit()
	st := openStoreOrExit(cfg)
	defer st.Close()
	if err := st.APIKeys().Revoke(context.Background(), id); err != nil {
		die("revoke: %v", err)
	}
	ok(os.Stdout, "revoked %s", id)
}
