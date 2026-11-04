package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/config"
)

// T10.21: `opod token create --node NAME` keeps NAME, in either order, and a
// node key with no name is still "node-join".
func TestTokenCreateKeepsTheNameBesideNode(t *testing.T) {
	for _, c := range []struct {
		args        []string
		name, scope string
	}{
		{[]string{"--node", "rack-7"}, "rack-7", "node"},
		{[]string{"rack-7", "--node"}, "rack-7", "node"},
		{[]string{"--node"}, "node-join", "node"},
		{[]string{"ci", "--admin"}, "ci", "admin"},
		{nil, "default", "user"},
	} {
		name, scope, _ := parseTokenCreate(c.args)
		if name != c.name || scope != c.scope {
			t.Errorf("%v → %s/%s, want %s/%s", c.args, name, scope, c.name, c.scope)
		}
	}
}

// T10.21: this host's leader URL is built from its listen address and its
// TLS — not "http://localhost"+listen, which broke for both.
func TestLocalLeaderURL(t *testing.T) {
	for listen, want := range map[string]string{
		"":               "http://localhost:8080",
		":8080":          "http://localhost:8080",
		"0.0.0.0:9000":   "http://localhost:9000",
		"[::]:9000":      "http://localhost:9000",
		"127.0.0.1:8081": "http://127.0.0.1:8081",
		"[::1]:8081":     "http://[::1]:8081",
		"leader.lan:80":  "http://leader.lan:80",
	} {
		if got := localLeaderURL(&config.Config{Listen: listen}); got != want {
			t.Errorf("listen %q → %s, want %s", listen, got, want)
		}
	}
	if got := localLeaderURL(&config.Config{Listen: ":8443", TLSCert: "c.pem", TLSKey: "k.pem"}); got != "https://localhost:8443" {
		t.Errorf("a TLS leader → %s", got)
	}
}

// T10.21: a free host:port reads as free (the colon used to be added in
// front of a host address, which made every such address "in use").
func TestPortAvailableTakesAHostAddress(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if portAvailable(addr) {
		t.Fatalf("%s is held and read as free", addr)
	}
	_ = ln.Close()
	if !portAvailable(addr) {
		t.Fatalf("%s is free and read as in use", addr)
	}
}

// T10.21: with no admin key on this host, a node command does not write the
// store while a leader answers — it says why instead.
func TestNodeCommandsRefuseTheStoreUnderALeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer srv.Close()
	cfg := &config.Config{DataDir: t.TempDir(), Listen: strings.TrimPrefix(srv.URL, "http://")}
	took, err := throughLeader(cfg, "POST", "/admin/v1/nodes/n1/drain")
	if !took || err == nil || !strings.Contains(err.Error(), "no admin key") {
		t.Fatalf("a leader answers and there is no key: took=%v err=%v", took, err)
	}
	srv.Close()
	if took, err := throughLeader(cfg, "POST", "/admin/v1/nodes/n1/drain"); took || err != nil {
		t.Fatalf("nobody answers: the store is the fallback (took=%v err=%v)", took, err)
	}
}

// T10.21: `opod config path` and the config view's edit hint name one file.
func TestTheEditHintNamesTheFileConfigPathPrints(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir() // an override the loader never read the file from
	if hint := config.ViewOf(cfg).EditHint; !strings.Contains(hint, loadedConfigPath()) {
		t.Fatalf("hint %q, config path %q", hint, loadedConfigPath())
	}
}

// T10.21: a --tp, --pp or --priority that is not a whole number stops the
// command with a message, where it used to read as 0. Run in a child: die exits.
func TestNonNumericIntFlagsAreRefused(t *testing.T) {
	if v := os.Getenv("OPOD_TEST_INTFLAG"); v != "" {
		extractIntFlag([]string{"--tp", v}, "tp")
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestNonNumericIntFlagsAreRefused$")
	cmd.Env = append(os.Environ(), "OPOD_TEST_INTFLAG=two")
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 || !strings.Contains(string(out), `--tp "two" is not a whole number`) {
		t.Fatalf("exit %v:\n%s", err, out)
	}
}

// T10.21: the top-level help names every cache verb, kv-prune included.
func TestHelpNamesKVPrune(t *testing.T) {
	var b strings.Builder
	printUsage(&b)
	if !strings.Contains(b.String(), "cache ls|prune|kv-prune") {
		t.Fatalf("opod --help:\n%s", b.String())
	}
}
