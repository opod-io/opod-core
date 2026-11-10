package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/opod-io/opod-core/internal/auth"
	"github.com/opod-io/opod-core/internal/store"
)

func cmdToken(args []string) {
	help := helpSpec{
		name:    "token",
		summary: "manage API keys and node-join tokens",
		usage:   "opod token <create [name] [--admin|--node] [--ttl D | --expires-at DATE] | ls | expire <id> [--in D] | renew <id> --ttl D | revoke <id>>",
		flagLines: []string{
			"--admin                   create: an admin-scope key (can call /admin/v1)",
			"--node                    create: a one-time join token for a new worker",
			"--ttl D                   create, renew: expire after D (30s, 5m, 2h, 7d; default: never)",
			"--expires-at DATE         create, renew: expire on DATE (YYYY-MM-DD or RFC3339)",
			"--in D                    expire: expire after D instead of now",
		},
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
		name, scope, expiresAt := parseTokenCreate(args[1:])
		tokenCreate(name, scope, expiresAt)
	case "expire":
		if len(args) < 2 {
			dieUsageOf("token", "token expire is missing an argument: opod token expire <id> [--in DURATION]")
		}
		tokenExpire(args[1], args[2:])
	case "renew":
		if len(args) < 2 {
			dieUsageOf("token", "token renew is missing an argument: opod token renew <id> --ttl DURATION | --expires-at DATE")
		}
		tokenRenew(args[1], args[2:])
	case "ls", "list":
		rest, asJSON := extractJSONFlag(args[1:])
		if len(rest) > 0 {
			dieUsageOf("token", "token ls takes only --json (got %q)", rest[0])
		}
		tokenList(asJSON)
	case "revoke":
		if len(args) < 2 {
			dieUsageOf("token", "token revoke is missing an argument: opod token revoke <id>")
		}
		tokenRevoke(args[1])
	default:
		dieUnknownSubcommand("token", args[0], []string{"create", "ls", "expire", "renew", "revoke"})
	}
}

func tokenCreate(name, scope string, expiresAt time.Time) {
	cfg := loadConfigOrExit()
	// A managed leader keeps no store of its own: this CLI's would be an
	// in-memory one, and the key would be "created" and gone.
	if cfg.Surfaces.Managed && os.Getenv("OPOD_STORAGE_DSN") == "" {
		die("this leader's keys are managed by its control plane (OPOD_MANAGED=1) — a key created here would be thrown away; create it in the control plane")
	}
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
	// The key alone on stdout, so key=$(opod token create …) captures it;
	// everything a person reads goes to stderr.
	ok(os.Stderr, "created %s (id=%s, scope=%s)", name, rec.ID, scope)
	if !expiresAt.IsZero() {
		fmt.Fprintf(os.Stderr, "  expires: %s (in %s)\n", expiresAt.Format(time.RFC3339), time.Until(expiresAt).Round(time.Second))
	}
	fmt.Fprintln(os.Stderr, "  the key below is shown once — store it now")
	if scope == "node" {
		fmt.Fprintf(os.Stderr, "  on the worker: opod join \"%s?token=<this key>\"\n", resolveBaseURL(cfg, ""))
	}
	if !isatty.IsTerminal(os.Stdout.Fd()) {
		warn("the key is going to a non-terminal (a pipe, a file, a CI log) — rotate it if that wasn't intended")
	}
	fmt.Println(plain)
}

func tokenList(asJSON bool) {
	cfg := loadConfigOrExit()
	st := openStoreOrExit(cfg)
	defer st.Close()
	keys, err := st.APIKeys().List(context.Background())
	if err != nil {
		die("list keys: %v", err)
	}
	now := time.Now()
	if asJSON {
		out := make([]map[string]string, 0, len(keys))
		for _, k := range keys {
			out = append(out, map[string]string{"id": k.ID, "name": k.Name, "scope": k.Scope, "state": keyState(k, now),
				"expires": keyExpiry(k), "created": k.CreatedAt.Format(time.RFC3339)})
		}
		emitJSON(out)
		return
	}
	if len(keys) == 0 {
		fmt.Println("(no API keys — create one with `opod token create`)")
		return
	}
	fmt.Printf("%-14s %-20s %-8s %-8s %-25s %s\n", "ID", "NAME", "SCOPE", "STATE", "EXPIRES", "CREATED")
	for _, k := range keys {
		fmt.Printf("%-14s %-20s %-8s %-8s %-25s %s\n",
			k.ID, k.Name, k.Scope, keyState(k, now), keyExpiry(k), k.CreatedAt.Format(time.RFC3339))
	}
}

// keyState is what the key does now: active, expired or revoked.
func keyState(k store.APIKey, now time.Time) string {
	switch {
	case k.Revoked:
		return "revoked"
	case !k.ExpiresAt.IsZero() && !k.ExpiresAt.After(now):
		return "expired"
	}
	return "active"
}

func keyExpiry(k store.APIKey) string {
	if k.ExpiresAt.IsZero() {
		return "never"
	}
	return k.ExpiresAt.Format(time.RFC3339)
}

// keyOrExit is the stored key with this id, or an error that names where the
// ids are.
func keyOrExit(st store.Store, id string) *store.APIKey {
	k, err := st.APIKeys().GetByID(context.Background(), id)
	if err != nil {
		die("read key %s: %v", id, err)
	}
	if k == nil {
		die("no key %s (see opod token ls)", id)
	}
	return k
}

func tokenRevoke(id string) {
	cfg := loadConfigOrExit()
	st := openStoreOrExit(cfg)
	defer st.Close()
	k := keyOrExit(st, id)
	if k.Revoked {
		ok(os.Stdout, "%s (%s) is already revoked — nothing changed", id, k.Name)
		return
	}
	if err := st.APIKeys().Revoke(context.Background(), id); err != nil {
		die("revoke %s: %v", id, err)
	}
	ok(os.Stdout, "revoked %s (%s, scope %s) — requests carrying it are refused now", id, k.Name, k.Scope)
}

// parseTokenCreate reads `opod token create`'s arguments: the name, the scope
// and the expiry.
func parseTokenCreate(args []string) (name, scope string, expiresAt time.Time) {
	name, scope = "default", "user"
	named := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--admin":
			scope = "admin"
		case "--node":
			scope = "node"
		case "--ttl":
			if i+1 >= len(args) {
				dieUsageOf("token", "--ttl needs a duration (30m, 2h, 7d)")
			}
			d, err := parseFlexibleDuration(args[i+1])
			if err != nil {
				dieUsageOf("token", "--ttl: %v", err)
			}
			expiresAt = time.Now().Add(d)
			i++
		case "--expires-at":
			if i+1 >= len(args) {
				dieUsageOf("token", "--expires-at needs a date: YYYY-MM-DD or RFC3339")
			}
			t, err := parseFlexibleDate(args[i+1])
			if err != nil {
				dieUsageOf("token", "--expires-at: %v", err)
			}
			expiresAt = t
			i++
		default:
			if strings.HasPrefix(a, "--ttl=") {
				d, err := parseFlexibleDuration(strings.TrimPrefix(a, "--ttl="))
				if err != nil {
					dieUsageOf("token", "--ttl: %v", err)
				}
				expiresAt = time.Now().Add(d)
				continue
			}
			if strings.HasPrefix(a, "--expires-at=") {
				t, err := parseFlexibleDate(strings.TrimPrefix(a, "--expires-at="))
				if err != nil {
					dieUsageOf("token", "--expires-at: %v", err)
				}
				expiresAt = t
				continue
			}
			if strings.HasPrefix(a, "-") {
				// It used to become the key's NAME: `--admn` minted a user key.
				msg := fmt.Sprintf("unknown token create flag %s", a)
				if g := suggest(strings.TrimLeft(a, "-"), []string{"admin", "node", "ttl", "expires-at"}); g != "" {
					msg += " — did you mean --" + g + "?"
				}
				dieUsageOf("token", "%s", msg)
			}
			if named {
				dieUsageOf("token", "token create takes one name (got %q and %q)", name, a)
			}
			name, named = a, true
		}
	}
	if !expiresAt.IsZero() && !expiresAt.After(time.Now()) {
		dieUsageOf("token", "the expiry %s is in the past — a key that expires before it is used is not a key", expiresAt.Format(time.RFC3339))
	}
	// A node key with no name is "node-join"; a name given before or after
	// --node is kept (T10.21: `--node NAME` dropped NAME).
	if scope == "node" && !named {
		name = "node-join"
	}
	return name, scope, expiresAt
}
