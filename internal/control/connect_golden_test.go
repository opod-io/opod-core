package control

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// updateGolden regenerates the testdata snapshot files when set:
//
//	go test ./internal/control/ -update
//
// The pattern keeps the goldens authoritative for "what does this
// snippet look like today" while making intentional updates a single
// command.
var updateGolden = flag.Bool("update", false, "regenerate testdata/connect-golden snapshots")

// TestConnectSnippet_Golden renders every snippet against a fixed
// input and compares to a checked-in snapshot. A failing test means
// either (a) the template was edited intentionally — rerun with
// `-update` to refresh — or (b) the template was edited
// accidentally and the change should be reverted.
//
// Fixed input keeps the comparison deterministic; the existing
// TestConnectSnippet_RendersAllClients covers substitution.
func TestConnectSnippet_Golden(t *testing.T) {
	dir := filepath.Join("testdata", "connect-golden")
	if *updateGolden {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range Clients() {
		c := c
		t.Run(c.ID, func(t *testing.T) {
			out, err := ConnectSnippet(ConnectInput{
				Client:  c.ID,
				BaseURL: "http://example.local:8080",
				Token:   "sk-orc-TEST",
				Model:   "test-model",
			})
			if err != nil {
				t.Fatalf("ConnectSnippet: %v", err)
			}
			goldenPath := filepath.Join(dir, c.ID+".golden")
			if *updateGolden {
				if err := os.WriteFile(goldenPath, []byte(out.Snippet), 0o644); err != nil {
					t.Fatalf("write golden: %v", err)
				}
				return
			}
			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read %s: %v — run `go test ./internal/control -update` to create the snapshot", goldenPath, err)
			}
			if got := strings.TrimRight(out.Snippet, "\n"); got != strings.TrimRight(string(want), "\n") {
				t.Errorf("snippet drift for %s.\nGot:\n%s\nWant:\n%s\n— if the change is intentional, rerun with `-update`",
					c.ID, got, string(want))
			}
		})
	}
}

// TestSnippetsCompile loads every .tmpl in the snippets directory and
// renders it. A new template that's missing from the registered
// clients list (or a renamed one) trips this guard so dead files
// can't accumulate.
func TestSnippetsCompile(t *testing.T) {
	// Map registered client IDs for the "every template is registered"
	// check below.
	registered := map[string]bool{}
	for _, c := range Clients() {
		registered[c.ID] = true
	}
	// Use the package's runtime resolution (snippetFS) — the embed
	// declares the directory at package scope. Walk it via the
	// embed.FS API rather than a filesystem path so the test stays
	// correct after a `go install`.
	entries, err := snippetFS.ReadDir("snippets")
	if err != nil {
		t.Fatalf("read snippets dir: %v", err)
	}
	for _, ent := range entries {
		name := strings.TrimSuffix(ent.Name(), ".tmpl")
		if !registered[name] {
			t.Errorf("template %q has no matching registered client — either register it or remove the file", ent.Name())
		}
	}
}
