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
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
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
	// livePID writes opod.pid naming a process that is alive (the test's own),
	// as a running leader leaves it.
	livePID bool
}

// answer is a stub that replies status and body to every request.
func answer(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

var transcripts = []transcriptCase{
	{name: "unknown-command", args: []string{"nodes"}},
	// A warning goes to stderr, with no colour in a pipe (rule 6).
	{name: "status-no-leader", args: []string{"status"}},
	// T24.76 — `opod up` says ready only once the port is its own.
	{name: "up-port-taken", args: []string{"up", "--no-wizard", "--auto-pull=false"}, stub: answer(http.StatusNotFound, ``)},
	{name: "up-already-running", args: []string{"up", "--no-wizard", "--auto-pull=false"}, livePID: true, stub: answer(http.StatusOK, `{"status":"ok"}`)},
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
	if c.livePID {
		if err := os.WriteFile(filepath.Join(data, "opod.pid"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestCLITranscripts$")
	cmd.Env = append(cleanEnv(), "OPOD_TRANSCRIPT=1", "OPOD_TRANSCRIPT_ARGS="+strings.Join(c.args, "\x1f"),
		"OPOD_DATA_DIR="+data, "HOME="+data, "OPOD_LISTEN="+listen, "NO_COLOR=1")
	for k, v := range c.env {
		cmd.Env = append(cmd.Env, k+"="+strings.ReplaceAll(v, "{{STUB}}", stubURL))
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("run %v: %v", c.args, err)
		}
		code = ee.ExitCode()
	}
	stable := func(s string) string {
		if stubURL != "" {
			s = strings.ReplaceAll(s, stubURL, "STUB")
			s = strings.ReplaceAll(s, strings.TrimPrefix(stubURL, "http://"), "STUB")
		}
		s = strings.ReplaceAll(s, "pid "+strconv.Itoa(os.Getpid()), "pid PID")
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
