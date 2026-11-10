package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mattn/go-isatty"

	"github.com/opod-io/opod-core/internal/config"
	"github.com/opod-io/opod-core/internal/engines"
	"github.com/opod-io/opod-core/internal/models"
	"github.com/opod-io/opod-core/internal/store"
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
	dsn := cfg.Storage.DSN
	// Managed leader (OPOD_MANAGED=1): the store is a rebuildable cache —
	// keys come from the auth snapshot, nodes from heartbeats, the manager
	// pulls usage + events by cursor. Nothing durable lands on the machine
	// unless the operator asks for it with OPOD_STORAGE_DSN.
	if cfg.Surfaces.Managed && os.Getenv("OPOD_STORAGE_DSN") == "" {
		dsn = ":memory:"
		cfg.Storage.DSN = dsn // what /admin/v1/config reports must be what runs
	}
	st, err := store.OpenSQLite(dsn)
	if err != nil {
		die("store: %v", err)
	}
	return st
}

func loadCatalogOrExit(cfg *config.Config) []models.Entry {
	entries, err := loadCatalog(cfg)
	if err != nil {
		die("catalog: %v", err)
	}
	return entries
}

// loadCatalog is the one way this binary reads its catalog: the embedded set,
// then the directories, every directory file under the operator's signature
// policy (OPOD_CATALOG_PUBKEY / OPOD_CATALOG_REQUIRE_SIGNED; models/trust.go).
func loadCatalog(cfg *config.Config) ([]models.Entry, error) {
	trust, err := models.NewCatalogTrust(cfg.Env.CatalogPubKey, cfg.Env.CatalogMustSign)
	if err != nil {
		return nil, err
	}
	return models.LoadCatalogTrusted(trust, cfg.CatalogDir, cfg.Env.CatalogDir)
}

// engineEndpoint is where the configured engine listens. The name is resolved
// through the driver registry first, so an alias ("sgl", "llama-cpp", "tt") is
// spelled in ONE place — the driver that declares it — and a linked driver
// cannot be missing here without TestEveryLinkedEngineHasAnEndpoint failing:
// for a week `sglang` was a registered driver this function did not know, and
// `opod up` refused it with an error that listed it as valid.
func engineEndpoint(cfg *config.Config, name string) (canonical, endpoint, apiKey string, ok bool) {
	canonical = engines.Canonical(name)
	switch canonical {
	case "ollama":
		return canonical, cfg.Engine.OllamaEndpoint, "", true
	case "vllm":
		return canonical, cfg.Engine.VLLMEndpoint, cfg.Engine.VLLMAPIKey, true
	case "mlx":
		return canonical, cfg.Engine.MLXEndpoint, "", true
	case "llamacpp":
		return canonical, cfg.Engine.LlamaCppEndpoint, "", true
	case "sglang":
		return canonical, cfg.Engine.SGLangEndpoint, "", true
	case "trtllm":
		return canonical, cfg.Engine.TRTLLMEndpoint, "", true
	}
	return canonical, "", "", false
}

func newEngineFromConfig(cfg *config.Config) engines.Engine {
	eng, err := engineFromConfig(cfg)
	if err != nil {
		die("%v", err)
	}
	return eng
}

// engineFromConfig is the configured engine, or why there is none: for a
// caller that reports instead of exiting (doctor).
func engineFromConfig(cfg *config.Config) (engines.Engine, error) {
	name := cfg.Engine.Preferred
	_, endpoint, apiKey, ok := engineEndpoint(cfg, name)
	if !ok {
		return nil, fmt.Errorf("unknown engine %q (valid: %s)", name, strings.Join(engines.Names(), ", "))
	}
	eng, err := engines.NewWithAuth(name, endpoint, apiKey)
	if err != nil {
		return nil, fmt.Errorf("engine: %w", err)
	}
	return eng, nil
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
//
// A value flag with no value after it — the last word, or followed by another
// flag — is an error: moved in front of the positionals it would take the
// first of them as its value (`connect curl --retries` read "curl").
func reorderFlagsFirst(args []string, valueFlags map[string]bool) ([]string, error) {
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
			if valueFlags[a] {
				if i+1 >= len(args) || (strings.HasPrefix(args[i+1], "-") && args[i+1] != "-") {
					return nil, fmt.Errorf("%s needs a value", a)
				}
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
	return append(flags, positionals...), nil
}

// resolveBaseURL returns the URL clients should point at, in this order:
//  1. explicit override (--base-url flag)
//  2. OPOD_BASE_URL env var
//  3. cfg.ExternalURL (if the operator set one in config)
//  4. this host's leader (localLeaderURL)
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
	return localLeaderURL(cfg)
}

// localLeaderURL is the base URL this host's leader answers on, from its
// config (T10.21: four callers built "http://localhost"+listen, which broke
// for a listen address with a host and for a leader serving TLS). An empty or
// unspecified host ("", 0.0.0.0, ::) is localhost; the scheme is https when
// the leader serves TLS (tls_cert and tls_key both set).
func localLeaderURL(cfg *config.Config) string {
	listen := cfg.Listen
	if listen == "" {
		listen = ":8080"
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		host, port = "", strings.TrimPrefix(listen, ":")
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "localhost"
	}
	scheme := "http"
	if cfg.TLSCert != "" && cfg.TLSKey != "" {
		scheme = "https"
	}
	return scheme + "://" + net.JoinHostPort(host, port)
}

// localLeaderClient is an HTTP client for this host's leader. When the
// leader serves TLS it trusts the leader's own certificate file — never
// "skip verification" — so a call to a TLS leader verifies.
func localLeaderClient(cfg *config.Config, timeout time.Duration) *http.Client {
	c := &http.Client{Timeout: timeout}
	if cfg.TLSCert == "" || cfg.TLSKey == "" {
		return c
	}
	pem, err := os.ReadFile(cfg.TLSCert)
	if err != nil {
		return c // the call then fails on the certificate, which names the problem
	}
	pool := x509.NewCertPool()
	if pool.AppendCertsFromPEM(pem) {
		c.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	}
	return c
}

// weightsOpTimeout is the client budget for admin calls that move model
// weights and boot engines (shard create, model add/load). On a cold cache
// these legitimately run tens of minutes; the old blanket 5-minute budget made
// the CLI hang up mid-create — which cancels the server's request context and
// strands a half-shard (rpc rows with no coordinator).
const weightsOpTimeout = 45 * time.Minute

// errNoAdminKey is adminCallT's answer when this host holds no admin key: the
// call reached nobody, which says nothing about whether a leader runs.
var errNoAdminKey = errors.New("no admin key")

// leaderAnswers reports whether this host's leader answers /healthz, a call
// that needs no key.
func leaderAnswers(cfg *config.Config) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, localLeaderURL(cfg)+"/healthz", nil)
	if err != nil {
		return false
	}
	resp, err := localLeaderClient(cfg, 3*time.Second).Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return true
}

// adminTarget is the leader an admin verb talks to and where that address
// came from: OPOD_BASE_URL when it is set — so an admin verb reaches a leader
// on another host — else this host's leader from its listen address. remote
// is true for the first: a store on this host is not that leader's store.
func adminTarget(cfg *config.Config) (url, source string, remote bool) {
	if v := strings.TrimSpace(os.Getenv("OPOD_BASE_URL")); v != "" {
		return strings.TrimRight(v, "/"), "from OPOD_BASE_URL", true
	}
	return localLeaderURL(cfg), "this host's listen address, from " + config.DefaultPath() + " or OPOD_LISTEN", false
}

// adminTargetURL is adminTarget's URL alone, for a message.
func adminTargetURL(cfg *config.Config) string {
	u, _, _ := adminTarget(cfg)
	return u
}

// adminKey is the key an admin verb sends and where it came from: OPOD_TOKEN,
// else the key `opod up` wrote on this host.
func adminKey(cfg *config.Config) (key, source string) {
	if v := strings.TrimSpace(os.Getenv("OPOD_TOKEN")); v != "" {
		return v, "OPOD_TOKEN"
	}
	return readLocalAdminKey(cfg), localAdminKeyPath(cfg)
}

// errUnreachable: the admin call reached no leader at all — nothing refused it.
// Only this error may send a command to its offline path (the store, the
// engine), and only for this host's own leader (unreachableError.Remote).
var errUnreachable = errors.New("no leader answering")

type unreachableError struct {
	URL, Source string
	Remote      bool
	Cause       error
}

func (e *unreachableError) Error() string {
	return fmt.Sprintf("no leader answering at %s (%s): %v — start it with `opod up`, or set OPOD_BASE_URL to the leader's URL", e.URL, e.Source, e.Cause)
}
func (e *unreachableError) Is(target error) bool { return target == errUnreachable }
func (e *unreachableError) Unwrap() error        { return e.Cause }

// localLeaderDown reports whether err says this host's leader is not running,
// the one case an offline fallback is right in.
func localLeaderDown(err error) bool {
	var ue *unreachableError
	return errors.As(err, &ue) && !ue.Remote
}

// adminError is a leader's refusal of an admin call, decoded from its
// {"error": {"type", "code", "message"}} body (or a plain string error).
type adminError struct {
	Method, Path, URL, KeySource string
	Status                       int
	Type, Code, Message          string
}

func (e *adminError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	if e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden {
		return fmt.Sprintf("the leader at %s refused the admin key from %s (%s) — run this on the leader's host, or set OPOD_TOKEN to an admin key (`opod token create ops --admin` on the leader)", e.URL, e.KeySource, msg)
	}
	code := e.Code
	if code == "" {
		code = e.Type
	}
	if code != "" {
		msg += " [" + code + "]"
	}
	return fmt.Sprintf("%s %s: the leader answered HTTP %d: %s", e.Method, e.Path, e.Status, msg)
}

// decodeAdminError reads a refusal body. A body that is not the leader's JSON
// (a proxy's HTML page, nothing at all) keeps the status and says so.
func decodeAdminError(status int, body []byte) (typ, code, msg string) {
	var nested struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &nested) == nil && nested.Error.Message != "" {
		return nested.Error.Type, nested.Error.Code, nested.Error.Message
	}
	var flat struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if json.Unmarshal(body, &flat) == nil && flat.Error != "" {
		return "", flat.Code, flat.Error
	}
	if b := strings.TrimSpace(string(body)); b != "" {
		if len(b) > 200 {
			b = b[:200] + "…"
		}
		return "", "", "not the leader's JSON: " + b
	}
	return "", "", "an empty body"
}

// adminCall makes an authenticated call to a leader's admin API (adminTarget)
// with the key adminKey finds, and returns the response body.
func adminCall(ctx context.Context, cfg *config.Config, method, path string, body []byte) ([]byte, error) {
	return adminCallT(ctx, cfg, method, path, body, 5*time.Minute)
}

// adminCallT is adminCall with an explicit client budget, for the admin ops
// that legitimately run long. Its errors are the three a caller acts on:
// errNoAdminKey (no key to send), *unreachableError (no leader answered —
// errUnreachable) and *adminError (a leader refused; the body is still
// returned for a caller that reads structure from it). A client budget that
// runs out is none of them: the leader may still be working.
func adminCallT(ctx context.Context, cfg *config.Config, method, path string, body []byte, timeout time.Duration) ([]byte, error) {
	base, source, remote := adminTarget(cfg)
	key, keySource := adminKey(cfg)
	if key == "" {
		return nil, fmt.Errorf("%w: none at %s and OPOD_TOKEN is not set — run this on the leader's host, or set OPOD_TOKEN to an admin key", errNoAdminKey, localAdminKeyPath(cfg))
	}
	var reader io.Reader = http.NoBody
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, reader)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	client := localLeaderClient(cfg, timeout)
	if remote {
		client = &http.Client{Timeout: timeout} // another host's certificate is not this host's file
	}
	resp, err := client.Do(req)
	if err != nil {
		var ne net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
			return nil, fmt.Errorf("gave up after %s waiting for %s %s at %s — the leader may still be working on it; check with `opod status`", timeout, method, path, base)
		}
		return nil, &unreachableError{URL: base, Source: source, Remote: remote, Cause: err}
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode >= 400 {
		typ, code, msg := decodeAdminError(resp.StatusCode, respBody)
		return respBody, &adminError{Method: method, Path: path, URL: base, KeySource: keySource, Status: resp.StatusCode, Type: typ, Code: code, Message: msg}
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
		warn("secret written to a non-terminal (pipe/redirect/CI) — it may be captured in logs or shell history; rotate it if that wasn't intended.")
	}
	fmt.Printf("    %s\n", secret)
}

func writePID(cfg *config.Config) error {
	return os.WriteFile(pidFilePath(cfg), []byte(strconv.Itoa(os.Getpid())), 0o644)
}

// processAlive reports whether pid names a running process (signal 0 asks
// without sending anything).
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	return err == nil && proc.Signal(syscall.Signal(0)) == nil
}

// processName is the command a pid runs, as ps names it ("" when it cannot
// be read).
func processName(pid int) string {
	out, err := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
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
	fmt.Fprintf(os.Stderr, "%s "+format+"\n", append([]any{ansiFor(os.Stderr, "31", "opod:")}, args...)...)
	os.Exit(1)
}

func note(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, ansiFor(w, "1;34", "▶")+" "+format+"\n", args...)
}

func ok(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, ansiFor(w, "1;32", "✔")+" "+format+"\n", args...)
}

// warn always writes to stderr (CONTRIBUTING.md "CLI output", rule 6): a
// warning on stdout ends up in a file or a pipe meant for the command's answer.
func warn(format string, args ...any) {
	fmt.Fprintf(os.Stderr, ansiFor(os.Stderr, "1;33", "⚠")+" "+format+"\n", args...)
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

// tickWhile says, every 10 s on stderr, that what started is still running
// and for how long (CONTRIBUTING.md "CLI output", rule 4). Call the returned
// func when it ends.
func tickWhile(what string) (stop func()) {
	start := time.Now()
	t := time.NewTicker(10 * time.Second)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case <-t.C:
				fmt.Fprintf(os.Stderr, "  … still %s (%s)\n", what, time.Since(start).Round(time.Second))
			}
		}
	}()
	return func() { t.Stop(); close(done) }
}
