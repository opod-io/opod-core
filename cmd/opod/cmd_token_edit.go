package main

// `opod token expire|renew` — flag parsing over the store's key mutations
// (expiry). The allowlist and rate-limit edits left with the per-key policy
// (ADR-077 §5, 2026-09-28).

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

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
