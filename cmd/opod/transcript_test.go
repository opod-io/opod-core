package main

// Golden CLI transcripts (the message standard, CONTRIBUTING.md "CLI output").
//
// A transcript is what a person sees: the exit code, stdout and stderr of one
// `opod` invocation. Each case runs the real main() — this test binary
// re-executed with the case's arguments, so os.Exit and the real output
// streams behave as they do for a user — against an httptest stub standing in
// for the leader, with a data directory of its own. The result is compared
// with testdata/cli/<name>.golden; `go test ./cmd/opod -run TestCLITranscripts
// -update` rewrites the files, and the diff is the review.

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/opod-io/opod-core/internal/store"
)

var updateTranscripts = flag.Bool("update", false, "rewrite testdata/cli/*.golden from the current output")

// transcriptCase is one invocation. stub answers the leader's routes; nil
// means no leader is listening at the configured address. key writes an admin
// key into the data directory, as `opod up` does. env adds variables; {{STUB}}
// in a value is the stub's URL. local false points this host's listen address
// at nothing, so only env can reach the stub.
type transcriptCase struct {
	name    string
	args    []string
	key     bool
	stub    http.HandlerFunc
	env     map[string]string
	noLocal bool
	// pidOf writes opod.pid naming a live process: "live" the test's own (a
	// leader that runs), "foreign" a `sleep` that inherited the number, "opod"
	// a child of this test binary that waits for SIGTERM as opod does.
	pidOf string
	// stopAfter sends SIGTERM this long after the start, for a command that
	// runs until stopped (opod join).
	stopAfter time.Duration
	// nodes are written into the case's store before the run (state.db in
	// its data directory), as a leader that has heard from workers holds them.
	nodes []store.Node
	keys  []store.APIKey // seeded into the store like nodes
}

// answer is a stub that replies status and body to every request.
func answer(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

// routes answers 200 with the body named for the path, 404 for any other.
func routes(bodies map[string]string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if b, ok := bodies[r.URL.Path]; ok {
			_, _ = w.Write([]byte(b))
			return
		}
		http.NotFound(w, r)
	}
}

// hardwareLine is this machine's own facts; goldens say HW.
var hardwareLine = regexp.MustCompile(`(?m)^(. (?:hardware|detected)): .*$`)

// gpuLine and installHint differ by machine and OS (CI runs macOS and Linux):
// a GPU line exists on some hosts only, and the install command is per OS.
var gpuLine = regexp.MustCompile(`(?m)^. GPU: .*\n`)
var installHint = regexp.MustCompile(`(→ install: ).*`)

// jsonLogLine is the structured log on stderr: timestamps and goroutine
// order make it differ on every run, and it is for machines. A golden pins
// the lines a person reads; the log keeps its own tests.
var jsonLogLine = regexp.MustCompile(`(?m)^(\{"time":|time=).*\n`)

// clockTime is a step printed in local time; the zone differs per runner.
var clockTime = regexp.MustCompile(`(?m)^(  )[0-9]{2}:[0-9]{2}:[0-9]{2}`)

// mintedKey and mintedID are what token create generates at random.
var mintedKey = regexp.MustCompile(`sk-orc-[A-Za-z0-9_-]{20,}`)
var mintedID = regexp.MustCompile(`\bk_[A-Za-z0-9_-]{11}\b`)

// pidNumber is a pid in a message; goldens say "pid N".
var pidNumber = regexp.MustCompile(`pid [0-9]+`)

var transcripts = []transcriptCase{
	{name: "unknown-command", args: []string{"nodes"}},
	// A warning goes to stderr, with no colour in a pipe (rule 6).
	{name: "status-no-leader", args: []string{"status"}},
	// T24.76 — `opod up` says ready only once the port is its own.
	{name: "up-port-taken", args: []string{"up", "--no-wizard", "--auto-pull=false"}, stub: answer(http.StatusNotFound, ``)},
	{name: "up-already-running", args: []string{"up", "--no-wizard", "--auto-pull=false"}, pidOf: "live", stub: answer(http.StatusOK, `{"status":"ok"}`)},
	// T24.78 — join says when it registered, and a stop it chose fails.
	{name: "join-registered", args: []string{"join", "{{STUB}}?token=join-token"}, stopAfter: 1500 * time.Millisecond,
		stub: routes(map[string]string{"/admin/v1/nodes/register": `{}`, "/admin/v1/nodes/heartbeat": `{}`}), noLocal: true,
		env: map[string]string{"OPOD_ADVERTISE_ADDR": "{{FREE}}", "OPOD_NODE_ID": "n_test"}},
	{name: "join-flags-before-url", args: []string{"join", "--gpu=1", "--vram-budget", "8", "{{STUB}}?token=join-token"}, stopAfter: 1500 * time.Millisecond,
		stub: routes(map[string]string{"/admin/v1/nodes/register": `{}`, "/admin/v1/nodes/heartbeat": `{}`}), noLocal: true,
		env: map[string]string{"OPOD_ADVERTISE_ADDR": "{{FREE}}", "OPOD_NODE_ID": "n_test"}},
	{name: "join-token-refused", args: []string{"join", "{{STUB}}?token=dead"}, stopAfter: 1500 * time.Millisecond,
		stub: answer(http.StatusUnauthorized, `{"error":{"type":"unauthorized","message":"invalid join token"}}`), noLocal: true,
		env: map[string]string{"OPOD_ADVERTISE_ADDR": "{{FREE}}", "OPOD_NODE_ID": "n_test"}},
	{name: "join-port-in-use", args: []string{"join", "{{STUB}}?token=join-token"}, stopAfter: 10 * time.Second,
		stub: routes(map[string]string{"/admin/v1/nodes/register": `{}`, "/admin/v1/nodes/heartbeat": `{}`}), noLocal: true,
		env: map[string]string{"OPOD_ADVERTISE_ADDR": "{{TAKEN}}", "OPOD_NODE_ID": "n_test"}},
	// T24.87 — cache: a wrong directory is said, empty says empty, one unit.
	{name: "cache-ls-empty", args: []string{"cache", "ls", "--dir", "{{DATA}}"}},
	{name: "cache-ls-extra-arg", args: []string{"cache", "ls", "--dir", "{{DATA}}", "x"}},
	{name: "cache-prune-dry-run", args: []string{"cache", "prune", "--dir", "{{DATA}}"}},
	{name: "cache-kv-prune-no-dir", args: []string{"cache", "kv-prune", "--max-gb", "1"}},
	{name: "cache-kv-prune-no-cap", args: []string{"cache", "kv-prune", "--dir", "{{DATA}}"}},
	{name: "cache-kv-prune-missing-dir", args: []string{"cache", "kv-prune", "--dir", "{{DATA}}/nosuch", "--max-gb", "1", "--apply"}},
	{name: "cache-kv-prune-missing-dir-json", args: []string{"cache", "kv-prune", "--dir", "{{DATA}}/nosuch", "--max-gb", "1", "--apply", "--json"}},
	{name: "cache-kv-prune-every-missing-dir", args: []string{"cache", "kv-prune", "--dir", "{{DATA}}/nosuch", "--max-gb", "1", "--every", "1s"}, stopAfter: 1500 * time.Millisecond},
	// T24.86 — image and catalog: the same answer in JSON and table, a line on success.
	{name: "image-ls-unknown-vendor", args: []string{"image", "ls", "--vendor", "foo"}},
	{name: "image-ls-unknown-vendor-json", args: []string{"image", "ls", "--vendor", "foo", "--json"}},
	{name: "image-ls-unknown-engine", args: []string{"image", "ls", "--engine", "mlx"}},
	{name: "image-ls-extra-arg", args: []string{"image", "ls", "vllm"}},
	{name: "image-show-short-name", args: []string{"image", "show", "vllm-nvidi"}},
	{name: "catalog-export", args: []string{"catalog", "export", "{{DATA}}/catalog"}},
	{name: "catalog-export-no-dir", args: []string{"catalog", "export"}},
	{name: "catalog-ls-extra-arg", args: []string{"catalog", "ls", "x"}},
	// T24.85 — token: an unknown flag never becomes a key, the key alone on stdout.
	{name: "token-create-typo-flag", args: []string{"token", "create", "--admn"}},
	{name: "token-create-key-alone-on-stdout", args: []string{"token", "create", "ci"}},
	{name: "token-create-node", args: []string{"token", "create", "--node"}},
	{name: "token-create-past-expiry", args: []string{"token", "create", "ci", "--expires-at", "2020-01-01"}},
	{name: "token-create-managed", args: []string{"token", "create", "ci"}, env: map[string]string{"OPOD_MANAGED": "1"}},
	{name: "token-revoke-repeat", args: []string{"token", "revoke", "k_seed1"}, keys: []store.APIKey{transcriptKey("k_seed1", true)}},
	{name: "token-revoke-active", args: []string{"token", "revoke", "k_seed1"}, keys: []store.APIKey{transcriptKey("k_seed1", false)}},
	{name: "token-revoke-unknown", args: []string{"token", "revoke", "k_nope"}},
	{name: "token-renew-revoked", args: []string{"token", "renew", "k_seed1", "--ttl", "7d"}, keys: []store.APIKey{transcriptKey("k_seed1", true)}},
	{name: "token-ls", args: []string{"token", "ls"}, keys: []store.APIKey{transcriptKey("k_seed1", true), transcriptKey("k_seed2", false)}},
	// T24.84 — node: a list for scripts, hints on not-found, "already" on repeats.
	{name: "node-ls-json-empty", args: []string{"node", "ls", "--json"}},
	{name: "node-ls-extra-arg", args: []string{"node", "ls", "extra"}},
	{name: "node-show-not-found", args: []string{"node", "show", "n_x"}},
	{name: "node-show-human", args: []string{"node", "show", "n_a"}, nodes: []store.Node{transcriptNode("n_a", store.NodeStateReady)}},
	{name: "node-drain-already", args: []string{"node", "drain", "n_a"}, nodes: []store.Node{transcriptNode("n_a", store.NodeStateDraining)}},
	{name: "node-undrain-not-drained", args: []string{"node", "undrain", "n_a"}, nodes: []store.Node{transcriptNode("n_a", store.NodeStateReady)}},
	// T24.83 — shard names the gang, warns on interrupt, checks flags first.
	{name: "shard-create-ready", args: []string{"shard", "create", "m", "2", "--nodes", "n_a,n_b", "--gang", "g1"}, key: true, stub: routes(map[string]string{
		"/admin/v1/nodes":         `[{"ID":"n_a"},{"ID":"n_b"}]`,
		"/admin/v1/shards/create": `{"status":"ready","model_id":"m"}`,
		"/admin/v1/shards":        `[{"ModelID":"m","GangID":"g1","Role":"coordinator","NodeID":"n_a"},{"ModelID":"m","GangID":"g1","Role":"rpc","NodeID":"n_b"}]`})},
	{name: "shard-create-interrupted", args: []string{"shard", "create", "m", "2", "--gang", "g1"}, key: true, stopAfter: 800 * time.Millisecond,
		stub: func(w http.ResponseWriter, r *http.Request) { time.Sleep(3 * time.Second) }},
	{name: "shard-create-unknown-node", args: []string{"shard", "create", "m", "--nodes", "n_zz"}, key: true, stub: routes(map[string]string{"/admin/v1/nodes": `[{"ID":"n_a"}]`})},
	{name: "shard-create-node-no-value", args: []string{"shard", "create", "m", "--node"}},
	{name: "shard-create-gang-no-value", args: []string{"shard", "create", "m", "--gang"}},
	{name: "shard-create-bogus-flag", args: []string{"shard", "create", "m", "--bogus"}},
	{name: "shard-ls-json", args: []string{"shard", "ls", "--json"}, key: true, stub: answer(http.StatusOK, `[{"ModelID":"m","GangID":"g1","Role":"coordinator","NodeID":"n_a"}]`)},
	{name: "shard-ls-not-a-list", args: []string{"shard", "ls"}, key: true, stub: answer(http.StatusOK, `<html>proxy</html>`)},
	{name: "shard-remove-nothing", args: []string{"shard", "remove", "m", "--yes"}, key: true, stub: answer(http.StatusOK, `[]`)},
	// T24.82 — model verbs name the model, the node and the next step.
	{name: "model-ps-unauthorized", args: []string{"model", "ps"}, key: true, stub: answer(http.StatusUnauthorized, `{"error":{"type":"unauthorized","message":"invalid admin key"}}`)},
	{name: "model-ps-not-json", args: []string{"model", "ps", "--json"}, key: true, stub: answer(http.StatusOK, `<html>proxy</html>`)},
	{name: "model-ps-no-residency", args: []string{"model", "ps"}, key: true, stub: answer(http.StatusOK, `{"supported":false}`)},
	{name: "model-move-not-json", args: []string{"model", "move", "m", "--from", "n_a", "--to", "n_b"}, key: true, stub: answer(http.StatusOK, `<html>proxy</html>`)},
	{name: "model-move-unfinished", args: []string{"model", "move", "m", "--from", "n_a", "--to", "n_b"}, key: true, stub: answer(http.StatusOK, `{"steps":[{"step":"started","at":"2026-11-09T00:00:00Z"}]}`)},
	{name: "model-add-node-placed", args: []string{"model", "add", "llama-3.2-1b", "--node", "n_a"}, key: true, stub: answer(http.StatusOK, `{"status":"placed"}`)},
	{name: "model-add-from-is-move", args: []string{"model", "add", "--from", "x.yaml"}},
	{name: "model-search-no-match", args: []string{"model", "search", "zzzz-nothing"}},
	{name: "model-search-bad-since", args: []string{"model", "search", "--since", "notadate"}},
	{name: "model-search-bad-sort", args: []string{"model", "search", "--sort", "size"}},
	// T24.81 — connect, disconnect, config, batch say what they need and did.
	{name: "connect-trailing-flag", args: []string{"connect", "curl", "--retries"}},
	{name: "connect-admin-key-warning", args: []string{"connect", "curl"}, key: true},
	{name: "disconnect-list-after-name", args: []string{"disconnect", "aider", "--list"}},
	{name: "config-path-not-created", args: []string{"config", "path"}},
	{name: "batch-run-no-dir", args: []string{"batch", "run", "--input", "in.jsonl", "--base-url", "http://127.0.0.1:1"}},
	{name: "batch-run-server-unreachable", args: []string{"batch", "run", "--input", "{{STUB}}/in.jsonl", "--base-url", "http://127.0.0.1:1", "--dir", "run", "--ready-timeout", "1s"},
		stub: answer(http.StatusOK, `{"custom_id":"a","method":"POST","url":"/v1/chat/completions","body":{}}`), noLocal: true},
	// T24.80 — update says what this build is, and replacing it asks first.
	// (A test binary is a development build; neither case reaches the network.)
	{name: "update-check-dev-build", args: []string{"update", "--check", "--version", "v0.4.5"}},
	{name: "update-pinned-no-tty", args: []string{"update", "--version", "v0.4.5"}},
	// T24.79 — doctor finishes the report and sets an exit code. PATH holds
	// only the system's own tools, so what this machine has installed does
	// not change the answer.
	{name: "doctor-all-pass", args: []string{"doctor"}, stub: routes(map[string]string{"/api/version": `{"version":"0.0.0"}`}), noLocal: true,
		env: map[string]string{"PATH": "/usr/bin:/bin", "OPOD_OLLAMA_ENDPOINT": "{{STUB}}", "OPOD_LISTEN": "{{FREE}}"}},
	{name: "doctor-engine-down", args: []string{"doctor"}, env: map[string]string{"PATH": "/usr/bin:/bin", "OPOD_LISTEN": "{{FREE}}"}},
	{name: "doctor-bad-engine", args: []string{"doctor"}, env: map[string]string{"PATH": "/usr/bin:/bin", "OPOD_ENGINE": "bogus", "OPOD_LISTEN": "{{FREE}}"}},
	// T24.77 — down stops only opod, says so, and a repeat is fine.
	{name: "down-not-running", args: []string{"down", "--no-unload"}},
	{name: "down-foreign-pid", args: []string{"down", "--no-unload"}, pidOf: "foreign"},
	{name: "down-stops-opod", args: []string{"down", "--no-unload"}, pidOf: "opod"},
	// T24.77 — status: only a 2xx is healthy; the engine is checked anyway.
	{name: "status-healthy-no-key", args: []string{"status"}, stub: answer(http.StatusOK, `{"status":"ok"}`)},
	{name: "status-healthy", args: []string{"status"}, key: true, stub: routes(map[string]string{"/healthz": `{"status":"ok"}`,
		"/admin/v1/models": `[{"CatalogID":"llama-3.2-1b","Status":"ready","Source":"ollama:llama3.2:1b"}]`, "/admin/v1/nodes": `[{"ID":"local","state":"ready"},{"ID":"n_a","state":"lost"}]`})},
	{name: "status-unauthorized", args: []string{"status"}, stub: answer(http.StatusUnauthorized, `{"error":"unauthorized"}`)},
	{name: "status-unavailable", args: []string{"status"}, stub: answer(http.StatusServiceUnavailable, `{"status":"draining"}`)},
	{name: "up-gateway-without-leader", args: []string{"up", "--role", "gateway", "--no-wizard"}},
	// T24.75 — with no terminal, a command that would ask names the fix.
	{name: "model-info-unknown-no-tty", args: []string{"model", "info", "nosuch"}},
	{name: "model-add-no-id-no-tty", args: []string{"model", "add"}},
	{name: "model-unload-not-installed-no-tty", args: []string{"model", "unload", "foo"}},
	{name: "model-load-bad-priority-no-tty", args: []string{"model", "load", "x", "--priority", "x"}},
	{name: "node-remove-no-tty", args: []string{"node", "remove", "n_one"}},
	{name: "shard-remove-no-tty", args: []string{"shard", "remove", "m"}},
	{name: "connect-unknown-no-tty", args: []string{"connect", "nosuch"}},
	// node ls reads this host's store, not the leader: an empty store says so.
	{name: "node-ls-empty-store", args: []string{"node", "ls"}},
	// shard ls asks the leader; a leader that refuses the key.
	{name: "shard-ls-unauthorized", args: []string{"shard", "ls"}, key: true, stub: answer(http.StatusUnauthorized, `{"error":{"type":"unauthorized","message":"invalid admin key"}}`)},
	{name: "shard-ls-forbidden", args: []string{"shard", "ls"}, key: true, stub: answer(http.StatusForbidden, `{"error":{"type":"forbidden","message":"admin scope required"}}`)},
	{name: "shard-ls-no-leader", args: []string{"shard", "ls"}, key: true},
	{name: "shard-ls-no-key", args: []string{"shard", "ls"}, stub: answer(http.StatusOK, `[]`)},
	// OPOD_BASE_URL reaches a leader on another host, with OPOD_TOKEN.
	{name: "shard-ls-base-url", args: []string{"shard", "ls"}, stub: answer(http.StatusOK, `[]`), noLocal: true,
		env: map[string]string{"OPOD_BASE_URL": "{{STUB}}", "OPOD_TOKEN": "transcript-admin-key"}},
	// A leader that answers 500 with no body took the call: the command must
	// not fall back to writing this host's store ("no such node" would be the
	// store path's answer).
	{name: "node-remove-500-no-body", args: []string{"node", "remove", "n_one", "--yes"}, key: true, stub: answer(http.StatusInternalServerError, ``)},
	{name: "node-drain-proxy-page", args: []string{"node", "drain", "n_one"}, key: true, stub: func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html><body>502 Bad Gateway</body></html>"))
	}},
}

func TestCLITranscripts(t *testing.T) {
	if os.Getenv("OPOD_TRANSCRIPT_WAIT") == "1" { // the "opod" pidOf child
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM)
		select {
		case <-sig:
		case <-time.After(30 * time.Second):
		}
		os.Exit(0)
	}
	if os.Getenv("OPOD_TRANSCRIPT") == "1" {
		os.Args = append([]string{"opod"}, strings.Split(os.Getenv("OPOD_TRANSCRIPT_ARGS"), "\x1f")...)
		main()
		os.Exit(0) // a command that returns without exiting succeeded
	}
	for _, c := range transcripts {
		t.Run(c.name, func(t *testing.T) {
			got := runTranscript(t, c)
			path := filepath.Join("testdata", "cli", c.name+".golden")
			if *updateTranscripts {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("no golden for %s (run with -update to write it): %v", c.name, err)
			}
			if got != string(want) {
				t.Errorf("transcript %s changed — review the output, then -update:\n--- want\n%s\n--- got\n%s", c.name, want, got)
			}
		})
	}
}

// runTranscript runs one case and renders it with the run's own addresses and
// paths replaced by stable names (STUB, $DATA).
func runTranscript(t *testing.T, c transcriptCase) string {
	t.Helper()
	data := t.TempDir()
	listen := "127.0.0.1:1" // nothing listens on port 1
	stubURL := ""
	if c.stub != nil {
		srv := httptest.NewServer(c.stub)
		t.Cleanup(srv.Close)
		stubURL = srv.URL
		if !c.noLocal {
			listen = strings.TrimPrefix(srv.URL, "http://")
		}
	}
	if c.key {
		if err := os.WriteFile(filepath.Join(data, "admin.key"), []byte("transcript-admin-key\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	foreign := 0 // a `sleep` the command must leave running
	if len(c.nodes)+len(c.keys) > 0 {
		st, err := store.OpenSQLite(filepath.Join(data, "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range c.nodes {
			if err := st.Nodes().Upsert(context.Background(), n); err != nil {
				t.Fatal(err)
			}
		}
		for _, k := range c.keys {
			if err := st.APIKeys().Create(context.Background(), k); err != nil {
				t.Fatal(err)
			}
		}
		_ = st.Close()
	}
	if c.pidOf != "" {
		pid := os.Getpid()
		if c.pidOf != "live" {
			child := exec.Command("sleep", "30")
			if c.pidOf == "opod" {
				child = exec.Command(os.Args[0], "-test.run=^TestCLITranscripts$")
				child.Env = append(cleanEnv(), "OPOD_TRANSCRIPT_WAIT=1")
			}
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			// Reaped as soon as it exits, as a shell reaps opod: an exited but
			// unreaped child still answers signal 0 and would read as running.
			go func() { _ = child.Wait() }()
			t.Cleanup(func() { _ = child.Process.Kill() })
			pid = child.Process.Pid
			if c.pidOf == "foreign" {
				foreign = pid
			}
			time.Sleep(200 * time.Millisecond) // let the child install its handler
		}
		if err := os.WriteFile(filepath.Join(data, "opod.pid"), []byte(strconv.Itoa(pid)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestCLITranscripts$")
	args := make([]string, len(c.args))
	for i, a := range c.args {
		args[i] = strings.ReplaceAll(strings.ReplaceAll(a, "{{STUB}}", stubURL), "{{DATA}}", data)
	}
	cmd.Env = append(cleanEnv(), "OPOD_TRANSCRIPT=1", "OPOD_TRANSCRIPT_ARGS="+strings.Join(args, "\x1f"),
		"OPOD_DATA_DIR="+data, "HOME="+data, "OPOD_LISTEN="+listen, "NO_COLOR=1",
		"OPOD_OLLAMA_ENDPOINT=http://127.0.0.1:1", // no engine answers, the same on every machine
		"TZ=UTC") // times print the same on every runner
	// {{TAKEN}} is an address something already listens on for the whole
	// run; {{FREE}} one nothing listens on.
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = taken.Close() })
	freeLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	free := freeLn.Addr().String()
	_ = freeLn.Close()
	for k, v := range c.env {
		v = strings.ReplaceAll(v, "{{STUB}}", stubURL)
		v = strings.ReplaceAll(v, "{{TAKEN}}", taken.Addr().String())
		v = strings.ReplaceAll(v, "{{FREE}}", free)
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	code := 0
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if c.stopAfter > 0 {
		stop := time.AfterFunc(c.stopAfter, func() { _ = cmd.Process.Signal(syscall.SIGTERM) })
		defer stop.Stop()
	}
	if err := cmd.Wait(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("run %v: %v", c.args, err)
		}
		code = ee.ExitCode()
	}
	if foreign != 0 && !processAlive(foreign) {
		t.Errorf("%v stopped pid %d, a process that is not opod", c.args, foreign)
	}
	stable := func(s string) string {
		if stubURL != "" {
			s = strings.ReplaceAll(s, stubURL, "STUB")
			s = strings.ReplaceAll(s, strings.TrimPrefix(stubURL, "http://"), "STUB")
		}
		s = pidNumber.ReplaceAllString(s, "pid N")
		s = mintedKey.ReplaceAllString(s, "sk-orc-KEY")
		s = mintedID.ReplaceAllString(s, "k_ID")
		s = hardwareLine.ReplaceAllString(s, "${1}: HW")
		s = clockTime.ReplaceAllString(s, "${1}HH:MM:SS")
		s = gpuLine.ReplaceAllString(s, "")
		s = installHint.ReplaceAllString(s, "${1}OS-HINT")
		s = jsonLogLine.ReplaceAllString(s, "")
		s = strings.ReplaceAll(s, taken.Addr().String(), "TAKEN")
		s = strings.ReplaceAll(s, free, "FREE")
		s = strings.ReplaceAll(s, "kill -9 "+strconv.Itoa(os.Getpid()), "kill -9 N")
		return strings.ReplaceAll(s, data, "$DATA")
	}
	var b strings.Builder
	b.WriteString("$ opod " + strings.Join(c.args, " ") + "\n")
	b.WriteString("--- exit " + strconv.Itoa(code) + "\n")
	b.WriteString("--- stdout\n" + stable(stdout.String()))
	b.WriteString("--- stderr\n" + stable(stderr.String()))
	return b.String()
}

// cleanEnv is the parent's environment without what would point the command
// somewhere else than the case says (a real leader, a real key, colour).
func cleanEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "OPOD_") || strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, "NO_COLOR=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// transcriptNode is a worker row with fixed facts, heard from long ago (so
// its live state is stable: lost unless drained).
func transcriptNode(id, state string) store.Node {
	return store.Node{ID: id, Hostname: "host-" + id, OS: "linux", Arch: "amd64", RAMGB: 64, Address: "192.0.2.10:8081",
		State: state, LastHeartbeat: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

// transcriptKey is a stored key with fixed facts; its hash matches no key.
func transcriptKey(id string, revoked bool) store.APIKey {
	return store.APIKey{ID: id, Hash: "hash-" + id, Name: "alice-" + id, Scope: "user",
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Revoked: revoked}
}
