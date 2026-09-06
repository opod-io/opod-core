package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mattn/go-isatty"

	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/models"
	"github.com/opod-io/opod/internal/store"
)

// loadConfigOrExit loads ~/.opod/config.yaml or the default. On failure
// it prints to stderr and exits with status 1.
func loadConfigOrExit() *config.Config {
	cfg, err := config.Load("")
	if err != nil {
		die("config: %v", err)
	}
	return cfg
}

// newLogger returns a JSON slog logger at the configured level.
func newLogger(cfg *config.Config) *slog.Logger {
	lvl := slog.LevelInfo
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

func openStoreOrExit(cfg *config.Config) store.Store {
	st, err := store.OpenSQLite(cfg.Storage.DSN)
	if err != nil {
		die("store: %v", err)
	}
	return st
}

func loadCatalogOrExit(cfg *config.Config) []models.Entry {
	entries, err := models.LoadCatalog(cfg.CatalogDir)
	if err != nil {
		die("catalog: %v", err)
	}
	return entries
}

func newEngineFromConfig(cfg *config.Config) engines.Engine {
	name := cfg.Engine.Preferred
	var endpoint, apiKey string
	switch name {
	case "ollama":
		endpoint = cfg.Engine.OllamaEndpoint
	case "vllm":
		endpoint = cfg.Engine.VLLMEndpoint
		apiKey = cfg.Engine.VLLMAPIKey
	case "mlx", "mlx-lm":
		endpoint = cfg.Engine.MLXEndpoint
	case "llamacpp", "llama-cpp", "llamacpp-rpc":
		endpoint = cfg.Engine.LlamaCppEndpoint
	default:
		die("unknown engine %q (valid: %s)", name, strings.Join(engines.Names(), ", "))
	}
	eng, err := engines.NewWithAuth(name, endpoint, apiKey)
	if err != nil {
		die("engine: %v", err)
	}
	return eng
}

func pidFilePath(cfg *config.Config) string {
	return filepath.Join(cfg.DataDir, "opod.pid")
}

// localAdminKeyPath is where bootstrapAdminKey persists the admin key so
// subsequent CLI invocations on this host can authenticate to the leader.
func localAdminKeyPath(cfg *config.Config) string {
	return filepath.Join(cfg.DataDir, "admin.key")
}

// readLocalAdminKey returns the saved admin key, or "" if missing.
func readLocalAdminKey(cfg *config.Config) string {
	data, err := os.ReadFile(localAdminKeyPath(cfg))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// resolveToken returns the API key to use for client commands, in this
// priority order:
//  1. explicit override (--token flag value passed in)
//  2. OPOD_TOKEN env var
//  3. saved admin key file (~/.opod/admin.key, written by `opod up`)
//
// Returns "" if none are set. Per user preference: env var wins over the
// file, so an operator can scope a command to a different token without
// editing the file.
func resolveToken(cfg *config.Config, override string) string {
	if override != "" {
		return override
	}
	if v := strings.TrimSpace(os.Getenv("OPOD_TOKEN")); v != "" {
		return v
	}
	return readLocalAdminKey(cfg)
}

// reorderFlagsFirst rewrites args so that any flags (and their values)
// come before any positional arguments. Go's stdlib `flag` package stops
// parsing at the first non-flag arg, which makes invocations like
// `opod connect cursor --model X` silently drop the trailing flags.
// This helper makes both orderings work without pulling in a third-party
// flag library.
//
// valueFlags is the set of flag names (with leading dashes) that take a
// value (--foo VALUE form). Boolean flags should not be listed here.
func reorderFlagsFirst(args []string, valueFlags map[string]bool) []string {
	flags := make([]string, 0, len(args))
	positionals := make([]string, 0, len(args))
	sawTerminator := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		// `--` ends flag parsing; everything after it is positional,
		// verbatim, in order — after any positionals seen before it.
		if a == "--" {
			sawTerminator = true
			positionals = append(positionals, args[i+1:]...)
			break
		}
		// A lone "-" conventionally means stdin — a positional, not a flag.
		if strings.HasPrefix(a, "-") && a != "-" {
			flags = append(flags, a)
			// --flag=value form: value is in the same token, no extra slot.
			if strings.Contains(a, "=") {
				continue
			}
			// --flag value form: also pick up the next token, if this flag
			// is known to take a value.
			if valueFlags[a] && i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
		} else {
			positionals = append(positionals, a)
		}
	}
	// Re-emit the terminator so the post-`--` args (and any pre-`--`
	// positionals, which were positional anyway) keep literal protection:
	// `pos1 --flag -- raw` → `--flag -- pos1 raw`.
	if sawTerminator {
		flags = append(flags, "--")
	}
	return append(flags, positionals...)
}

// resolveBaseURL returns the URL clients should point at, in this order:
//  1. explicit override (--base-url flag)
//  2. OPOD_BASE_URL env var
//  3. cfg.ExternalURL (if the operator set one in config)
//  4. http://localhost + cfg.Listen
func resolveBaseURL(cfg *config.Config, override string) string {
	if override != "" {
		return strings.TrimRight(override, "/")
	}
	if v := strings.TrimSpace(os.Getenv("OPOD_BASE_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	if cfg.ExternalURL != "" {
		return strings.TrimRight(cfg.ExternalURL, "/")
	}
	listen := cfg.Listen
	if listen == "" {
		listen = ":8080"
	}
	return "http://localhost" + listen
}

// weightsOpTimeout is the client budget for admin calls that move model
// weights and boot engines (shard create, model add/load). On a cold cache
// these legitimately run tens of minutes; the old blanket 5-minute budget made
// the CLI hang up mid-create — which cancels the server's request context and
// strands a half-shard (rpc rows with no coordinator).
const weightsOpTimeout = 45 * time.Minute

// adminCall makes an authenticated HTTP request to the local leader's admin
// API. Returns the response body bytes. If the leader isn't running this
// returns a clear error rather than a confusing dial failure.
func adminCall(ctx context.Context, cfg *config.Config, method, path string, body []byte) ([]byte, error) {
	return adminCallT(ctx, cfg, method, path, body, 5*time.Minute)
}

// adminCallT is adminCall with an explicit client budget, for the admin ops
// that legitimately run long.
func adminCallT(ctx context.Context, cfg *config.Config, method, path string, body []byte, timeout time.Duration) ([]byte, error) {
	key := readLocalAdminKey(cfg)
	if key == "" {
		return nil, fmt.Errorf("no admin key on disk at %s — is `opod up` running on this host?", localAdminKeyPath(cfg))
	}
	listen := cfg.Listen
	if listen == "" {
		listen = ":8080"
	}
	url := "http://localhost" + listen + path
	var reader *bytes.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	var req *http.Request
	var err error
	if reader != nil {
		req, err = http.NewRequestWithContext(ctx, method, url, reader)
	} else {
		req, err = http.NewRequestWithContext(ctx, method, url, http.NoBody)
	}
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("admin call: %w (is opod up running?)", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return respBody, fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	return respBody, nil
}

// emitSecret prints a one-time secret (an API/admin key). On an interactive
// terminal it just prints the value. When stdout is NOT a TTY — piped,
// redirected, or running under CI — it first warns on stderr that the secret
// is landing in a non-terminal sink (shell history, CI logs, a file) so the
// operator can rotate it if that wasn't intended. The value still goes to
// stdout so `key=$(opod token create ...)` automation keeps working.
func emitSecret(secret string) {
	if !isatty.IsTerminal(os.Stdout.Fd()) {
		warn(os.Stderr, "secret written to a non-terminal (pipe/redirect/CI) — it may be captured in logs or shell history; rotate it if that wasn't intended.")
	}
	fmt.Printf("    %s\n", secret)
}

func writePID(cfg *config.Config) error {
	return os.WriteFile(pidFilePath(cfg), []byte(strconv.Itoa(os.Getpid())), 0o644)
}

func readPID(cfg *config.Config) (int, error) {
	data, err := os.ReadFile(pidFilePath(cfg))
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

func removePID(cfg *config.Config) {
	_ = os.Remove(pidFilePath(cfg))
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s "+format+"\n", append([]any{red("opod:")}, args...)...)
	os.Exit(1)
}

func note(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, "\033[1;34m▶\033[0m "+format+"\n", args...)
}

func ok(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, "\033[1;32m✔\033[0m "+format+"\n", args...)
}

func warn(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, "\033[1;33m⚠\033[0m "+format+"\n", args...)
}

func truncStr(s string, n int) string {
	if n <= 0 {
		return ""
	}
	// Count and slice by rune so a multibyte character isn't split into
	// mojibake, and guard the n==1 edge (n-1 == 0).
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
