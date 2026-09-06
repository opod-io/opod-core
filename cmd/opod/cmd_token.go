package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/opod-io/opod/internal/auth"
)

func cmdToken(args []string) {
	help := helpSpec{
		name:    "token",
		summary: "manage API keys and node-join tokens",
		usage:   "opod token <create [name] [--admin|--node] [--models a,b,…] | ls | edit <id> ... | expire <id> [--in D] | renew <id> --ttl D | budget <add|ls|rm> <id> ... | revoke <id>>",
		examples: []string{
			"opod token create alice                            # user-scope key for dev `alice`",
			"opod token create alice-admin --admin              # admin-scope key (can call /admin/v1/*)",
			"opod token create alice --models qwen-coder-7b     # restrict to one model",
			"opod token create bob   --models 'claude-*,gpt-*'  # vendor families via glob",
			"opod token create alice --rpm 60 --tpm 100000      # per-minute ceilings",
			"opod token create alice --ttl 7d                   # auto-expire in 7 days",
			"opod token create alice --expires-at 2026-07-01    # absolute expiry date",
			"opod token expire k_abc                            # expire immediately",
			"opod token expire k_abc --in 1h                    # expire in 1 hour",
			"opod token renew  k_abc --ttl 30d                  # extend expiry by 30 days from now",
			"opod token create --node                           # one-time join token for a new worker",
			"opod token edit k_abc123 --add-model qwen3-14b     # extend the allowlist",
			"opod token edit k_abc123 --remove-model gpt-4o     # tighten the allowlist",
			"opod token edit k_abc123 --set-models a,b,c        # replace the allowlist",
			"opod token edit k_abc123 --clear-models            # drop the allowlist (any model)",
			"opod token edit k_abc123 --rpm 30 --tpm 50000      # set per-minute ceilings (0 = unlimited)",
			"opod token budget add k_abc --window month --limit 100 --unit usd  # $100/mo cap",
			"opod token budget add k_abc --window day --limit 1000000 --unit tokens",
			"opod token budget ls k_abc                         # show active budgets + utilization",
			"opod token budget rm k_abc 4                       # drop budget #4",
			"opod token ls",
			"opod token revoke k_abc123",
		},
		notes: []string{
			"⚠️  --node tokens are the shared secret leader ↔ worker — only issue on a trusted network (LAN or Tailscale).",
			"`--models` accepts a comma-separated list. Entries support a `*` suffix wildcard (`claude-*`).",
			"A key with no allowlist can call any model. An empty allowlist (`--set-models ''`) denies every model.",
			"`--rpm` (requests/min) and `--tpm` (tokens/min) are in-memory leaky buckets; reset on leader restart. 0 = unlimited.",
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
		var models []string
		rpm, tpm := 0, 0
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
			case "--models":
				if i+1 >= len(args) {
					die("--models requires a comma-separated list (e.g. --models qwen3-14b,claude-*)")
				}
				models = parseModelList(args[i+1])
				i++
			case "--rpm":
				if i+1 >= len(args) {
					die("--rpm requires a value (0 = unlimited)")
				}
				rpm = parseIntFlag(args[i+1], "--rpm")
				i++
			case "--tpm":
				if i+1 >= len(args) {
					die("--tpm requires a value (0 = unlimited)")
				}
				tpm = parseIntFlag(args[i+1], "--tpm")
				i++
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
				if strings.HasPrefix(a, "--models=") {
					models = parseModelList(strings.TrimPrefix(a, "--models="))
					continue
				}
				if strings.HasPrefix(a, "--rpm=") {
					rpm = parseIntFlag(strings.TrimPrefix(a, "--rpm="), "--rpm")
					continue
				}
				if strings.HasPrefix(a, "--tpm=") {
					tpm = parseIntFlag(strings.TrimPrefix(a, "--tpm="), "--tpm")
					continue
				}
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
		tokenCreate(name, scope, models, rpm, tpm, expiresAt)
	case "edit":
		if len(args) < 2 {
			die("usage: opod token edit <id> --add-model X | --remove-model Y | --set-models a,b,c | --clear-models")
		}
		tokenEdit(args[1], args[2:])
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
	case "budget":
		tokenBudget(args[1:])
	case "ls", "list":
		tokenList()
	case "revoke":
		if len(args) < 2 {
			die("usage: opod token revoke <id>")
		}
		tokenRevoke(args[1])
	default:
		dieUnknownSubcommand("token", args[0], []string{"create", "ls", "edit", "expire", "renew", "budget", "revoke"})
	}
}

// parseIntFlag parses a non-negative integer for a token-create flag.
// Centralized so the error message stays consistent ("invalid --rpm",
// not whatever strconv defaults to).
func parseIntFlag(s, flag string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		die("invalid %s: %q (expected a non-negative integer)", flag, s)
	}
	return n
}

// parseModelList splits a comma-separated list, trims whitespace, and
// drops empties. Returns nil for an empty input — callers distinguish
// nil (no flag) from []string{} (explicit deny-all via the API).
func parseModelList(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func tokenCreate(name, scope string, models []string, rpm, tpm int, expiresAt time.Time) {
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
	rec.AllowedModels = models
	rec.RPMLimit = rpm
	rec.TPMLimit = tpm
	rec.ExpiresAt = expiresAt
	if err := st.APIKeys().Create(context.Background(), rec); err != nil {
		die("persist key: %v", err)
	}
	ok(os.Stdout, "created %s (id=%s, scope=%s)", name, rec.ID, scope)
	if len(models) > 0 {
		fmt.Printf("  allowed models: %s\n", strings.Join(models, ", "))
	}
	if rpm > 0 || tpm > 0 {
		fmt.Printf("  rpm: %s · tpm: %s\n", limitStr(rpm), limitStr(tpm))
	}
	if !expiresAt.IsZero() {
		fmt.Printf("  expires: %s (in %s)\n", expiresAt.Format(time.RFC3339), time.Until(expiresAt).Round(time.Second))
	}
	fmt.Println()
	fmt.Println("  Key (shown once — store it now):")
	emitSecret(plain)
}

// tokenExpire pushes a key's expiry to a specific point in time. With
// no flag the key expires immediately (the next request gets 401
// key_expired). `--in DURATION` sets the expiry to now + duration.
func tokenExpire(id string, args []string) {
	expiresAt := time.Now() // default: expire now
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--in":
			if i+1 >= len(args) {
				die("--in requires a duration (e.g. 1h, 30m, 7d)")
			}
			d, err := parseFlexibleDuration(args[i+1])
			if err != nil {
				die("invalid --in: %v", err)
			}
			expiresAt = time.Now().Add(d)
			i++
		default:
			if strings.HasPrefix(a, "--in=") {
				d, err := parseFlexibleDuration(strings.TrimPrefix(a, "--in="))
				if err != nil {
					die("invalid --in: %v", err)
				}
				expiresAt = time.Now().Add(d)
				continue
			}
			die("unknown flag: %s", a)
		}
	}
	cfg := loadConfigOrExit()
	st := openStoreOrExit(cfg)
	defer st.Close()
	if err := st.APIKeys().UpdateExpiresAt(context.Background(), id, expiresAt); err != nil {
		die("update expires_at: %v", err)
	}
	if expiresAt.Before(time.Now().Add(time.Second)) {
		ok(os.Stdout, "%s: expired immediately", id)
	} else {
		ok(os.Stdout, "%s: will expire at %s (in %s)", id,
			expiresAt.Format(time.RFC3339), time.Until(expiresAt).Round(time.Second))
	}
}

// tokenRenew extends a key's expiry by --ttl from NOW (not from the
// existing expiry) so a forgotten renewal stays predictable.
// `--expires-at` is also supported for absolute dates.
func tokenRenew(id string, args []string) {
	var expiresAt time.Time
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--ttl":
			if i+1 >= len(args) {
				die("--ttl requires a duration")
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
			die("unknown flag: %s", a)
		}
	}
	if expiresAt.IsZero() {
		die("renew needs --ttl or --expires-at")
	}
	cfg := loadConfigOrExit()
	st := openStoreOrExit(cfg)
	defer st.Close()
	if err := st.APIKeys().UpdateExpiresAt(context.Background(), id, expiresAt); err != nil {
		die("update expires_at: %v", err)
	}
	ok(os.Stdout, "%s: renewed — expires %s (in %s)", id,
		expiresAt.Format(time.RFC3339), time.Until(expiresAt).Round(time.Second))
}

// parseFlexibleDuration accepts standard Go durations ("30s", "5m",
// "2h") plus a `d` (days) extension that the stdlib doesn't.
// "7d" → 7×24h.
func parseFlexibleDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	if strings.HasSuffix(s, "d") {
		nStr := strings.TrimSuffix(s, "d")
		n, err := strconv.Atoi(nStr)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("invalid days: %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

// parseFlexibleDate accepts YYYY-MM-DD or RFC3339. Dates are treated
// as midnight UTC so `--expires-at 2026-07-01` means the very start of
// July 1.
func parseFlexibleDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("expected YYYY-MM-DD or RFC3339, got %q", s)
}

func limitStr(n int) string {
	if n <= 0 {
		return "∞"
	}
	return fmt.Sprintf("%d", n)
}

// tokenEdit currently supports only allowlist edits — that's the one
// editable field today. Add/remove deltas are applied to the existing
// list; --set-models replaces it; --clear-models drops the restriction
// entirely.
func tokenEdit(id string, args []string) {
	cfg := loadConfigOrExit()
	st := openStoreOrExit(cfg)
	defer st.Close()
	key, err := st.APIKeys().GetByID(context.Background(), id)
	if err != nil {
		die("lookup %s: %v", id, err)
	}
	if key == nil {
		die("no such token: %s", id)
	}

	current := append([]string(nil), key.AllowedModels...)
	hadOriginalList := key.AllowedModels != nil
	hadList := hadOriginalList
	addUsed, removeUsed := false, false
	var setList []string
	setExplicit := false
	clearRestriction := false
	rpm, tpm := key.RPMLimit, key.TPMLimit
	rpmSet, tpmSet := false, false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--add-model":
			if i+1 >= len(args) {
				die("--add-model requires a model id")
			}
			current = appendUnique(current, args[i+1])
			hadList = true
			addUsed = true
			i++
		case "--remove-model":
			if i+1 >= len(args) {
				die("--remove-model requires a model id")
			}
			current = removeOne(current, args[i+1])
			hadList = true
			removeUsed = true
			i++
		case "--set-models":
			if i+1 >= len(args) {
				die("--set-models requires a comma-separated list (use --clear-models to drop the restriction)")
			}
			setList = parseModelList(args[i+1])
			setExplicit = true
			i++
		case "--clear-models":
			clearRestriction = true
		case "--rpm":
			if i+1 >= len(args) {
				die("--rpm requires a value (0 = unlimited)")
			}
			rpm = parseIntFlag(args[i+1], "--rpm")
			rpmSet = true
			i++
		case "--tpm":
			if i+1 >= len(args) {
				die("--tpm requires a value (0 = unlimited)")
			}
			tpm = parseIntFlag(args[i+1], "--tpm")
			tpmSet = true
			i++
		default:
			if strings.HasPrefix(a, "--add-model=") {
				current = appendUnique(current, strings.TrimPrefix(a, "--add-model="))
				hadList = true
				addUsed = true
				continue
			}
			if strings.HasPrefix(a, "--remove-model=") {
				current = removeOne(current, strings.TrimPrefix(a, "--remove-model="))
				hadList = true
				removeUsed = true
				continue
			}
			if strings.HasPrefix(a, "--set-models=") {
				setList = parseModelList(strings.TrimPrefix(a, "--set-models="))
				setExplicit = true
				continue
			}
			if strings.HasPrefix(a, "--rpm=") {
				rpm = parseIntFlag(strings.TrimPrefix(a, "--rpm="), "--rpm")
				rpmSet = true
				continue
			}
			if strings.HasPrefix(a, "--tpm=") {
				tpm = parseIntFlag(strings.TrimPrefix(a, "--tpm="), "--tpm")
				tpmSet = true
				continue
			}
			die("unknown flag: %s", a)
		}
	}

	allowlistChange := clearRestriction || setExplicit || hadList
	rateChange := rpmSet || tpmSet
	if !allowlistChange && !rateChange {
		die("no edit flag given (try --add-model, --remove-model, --set-models, --clear-models, --rpm, --tpm)")
	}

	// A key with NO allowlist allows every model. Removing from "all
	// models" can't produce a sensible list — the naive result would be
	// an empty list, i.e. deny-all. Refuse unless this invocation also
	// defines a list (--set-models / --add-model / --clear-models).
	if removeUsed && !hadOriginalList && !setExplicit && !addUsed && !clearRestriction {
		die("%s allows all models (no allowlist) — removing one would deny EVERY model; use --set-models to define an allowlist first", id)
	}

	if allowlistChange {
		var newAllowed []string
		switch {
		case clearRestriction:
			newAllowed = nil
		case setExplicit:
			if setList == nil {
				newAllowed = []string{} // explicit empty = deny all
			} else {
				newAllowed = setList
			}
		case hadList:
			newAllowed = current
			if newAllowed == nil {
				newAllowed = []string{}
			}
		}
		if err := st.APIKeys().UpdateAllowedModels(context.Background(), id, newAllowed); err != nil {
			die("update allowed_models: %v", err)
		}
		switch {
		case newAllowed == nil:
			ok(os.Stdout, "%s: allowlist cleared (any model allowed)", id)
		case len(newAllowed) == 0:
			ok(os.Stdout, "%s: allowlist now denies every model", id)
		default:
			ok(os.Stdout, "%s: allowed models = %s", id, strings.Join(newAllowed, ", "))
			if addUsed && !hadOriginalList && !setExplicit {
				warn(os.Stdout, "%s previously allowed ALL models — it is now RESTRICTED to the list above (undo with --clear-models)", id)
			}
		}
	}
	if rateChange {
		if err := st.APIKeys().UpdateRateLimits(context.Background(), id, rpm, tpm); err != nil {
			die("update rate limits: %v", err)
		}
		ok(os.Stdout, "%s: rpm = %s · tpm = %s", id, limitStr(rpm), limitStr(tpm))
	}
}

func appendUnique(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

func removeOne(list []string, v string) []string {
	out := list[:0]
	for _, x := range list {
		if x != v {
			out = append(out, x)
		}
	}
	return out
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
	fmt.Printf("%-14s %-20s %-8s %-7s %-8s %-8s %-30s %s\n", "ID", "NAME", "SCOPE", "REVOKED", "RPM", "TPM", "MODELS", "CREATED")
	for _, k := range keys {
		rev := "no"
		if k.Revoked {
			rev = "yes"
		}
		models := "any"
		switch {
		case k.AllowedModels == nil:
			// unrestricted; render as "any"
		case len(k.AllowedModels) == 0:
			models = "(deny all)"
		default:
			models = strings.Join(k.AllowedModels, ",")
			if len(models) > 28 {
				models = models[:27] + "…"
			}
		}
		fmt.Printf("%-14s %-20s %-8s %-7s %-8s %-8s %-30s %s\n",
			k.ID, k.Name, k.Scope, rev,
			limitStr(k.RPMLimit), limitStr(k.TPMLimit),
			models, k.CreatedAt.Format(time.RFC3339))
	}
}

// tokenBudget dispatches the `opod token budget` subcommands. All
// three (add/ls/rm) hit the admin API rather than the store directly
// so they work against a remote leader the same way the dashboard
// does.
func tokenBudget(args []string) {
	if len(args) == 0 {
		die("usage: opod token budget <add|ls|rm> <key-id> ...")
	}
	switch args[0] {
	case "add":
		if len(args) < 2 {
			die("usage: opod token budget add <key-id> --window day|week|month --limit N --unit tokens|usd")
		}
		tokenBudgetAdd(args[1], args[2:])
	case "ls", "list":
		if len(args) < 2 {
			die("usage: opod token budget ls <key-id>")
		}
		tokenBudgetList(args[1])
	case "rm", "remove", "delete":
		if len(args) < 3 {
			die("usage: opod token budget rm <key-id> <budget-id>")
		}
		tokenBudgetRemove(args[1], args[2])
	default:
		dieUnknownSubcommand("token budget", args[0], []string{"add", "ls", "rm"})
	}
}

func tokenBudgetAdd(keyID string, args []string) {
	window, unit := "", ""
	var limit float64
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--window":
			if i+1 < len(args) {
				window = args[i+1]
				i++
			}
		case "--limit":
			if i+1 < len(args) {
				v, err := strconv.ParseFloat(args[i+1], 64)
				if err != nil {
					die("invalid --limit %q", args[i+1])
				}
				limit = v
				i++
			}
		case "--unit":
			if i+1 < len(args) {
				unit = args[i+1]
				i++
			}
		default:
			if strings.HasPrefix(a, "--window=") {
				window = strings.TrimPrefix(a, "--window=")
				continue
			}
			if strings.HasPrefix(a, "--limit=") {
				v, err := strconv.ParseFloat(strings.TrimPrefix(a, "--limit="), 64)
				if err != nil {
					die("invalid --limit %q", a)
				}
				limit = v
				continue
			}
			if strings.HasPrefix(a, "--unit=") {
				unit = strings.TrimPrefix(a, "--unit=")
				continue
			}
			die("unknown flag: %s", a)
		}
	}
	if window == "" || unit == "" || limit <= 0 {
		die("--window, --limit (>0), and --unit are all required")
	}
	cfg := loadConfigOrExit()
	// Build the body with json.Marshal — fmt.Sprintf with %q escapes for Go
	// string literals, not JSON, so a window/unit containing certain bytes
	// could produce malformed JSON.
	body, err := json.Marshal(map[string]any{
		"window":      window,
		"limit_unit":  unit,
		"limit_value": limit,
	})
	if err != nil {
		die("encode budget: %v", err)
	}
	resp, err := adminCall(context.Background(), cfg, "POST", "/admin/v1/tokens/"+url.PathEscape(keyID)+"/budgets", body)
	if err != nil {
		die("%v: %s", err, string(resp))
	}
	ok(os.Stdout, "added %s/%s budget — limit %v (key %s)", window, unit, limit, keyID)
}

func tokenBudgetList(keyID string) {
	cfg := loadConfigOrExit()
	body, err := adminCall(context.Background(), cfg, "GET", "/admin/v1/tokens/"+keyID+"/budgets", nil)
	if err != nil {
		die("%v: %s", err, string(body))
	}
	type budgetView struct {
		ID           int64     `json:"ID"`
		Window       string    `json:"Window"`
		LimitUnit    string    `json:"LimitUnit"`
		LimitValue   float64   `json:"LimitValue"`
		CurrentValue float64   `json:"CurrentValue"`
		ResetAt      time.Time `json:"ResetAt"`
	}
	var bs []budgetView
	if err := json.Unmarshal(body, &bs); err != nil {
		die("decode budgets: %v", err)
	}
	if len(bs) == 0 {
		fmt.Println("(no budgets attached)")
		return
	}
	fmt.Printf("%-4s %-8s %-8s %15s %15s %-6s %s\n", "ID", "WINDOW", "UNIT", "LIMIT", "CURRENT", "USED", "RESETS")
	for _, b := range bs {
		pct := 0.0
		if b.LimitValue > 0 {
			pct = 100 * b.CurrentValue / b.LimitValue
		}
		fmt.Printf("%-4d %-8s %-8s %15s %15s %5.1f%% %s\n",
			b.ID, b.Window, b.LimitUnit,
			fmt.Sprintf("%.4f", b.LimitValue),
			fmt.Sprintf("%.4f", b.CurrentValue),
			pct, b.ResetAt.Format(time.RFC3339))
	}
}

func tokenBudgetRemove(keyID, budgetID string) {
	cfg := loadConfigOrExit()
	body, err := adminCall(context.Background(), cfg, "DELETE",
		fmt.Sprintf("/admin/v1/tokens/%s/budgets/%s", keyID, budgetID), nil)
	if err != nil {
		die("%v: %s", err, string(body))
	}
	ok(os.Stdout, "removed budget %s from key %s", budgetID, keyID)
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
