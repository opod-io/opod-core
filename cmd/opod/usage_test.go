package main

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The did-you-mean list and `opod help <cmd>` read topLevelCommands; main()
// dispatches with its own switch. They are one list or a command goes missing
// from one of them (cache was, 2026-11-09).
func TestTopLevelCommandsIsTheDispatchSwitch(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var cases []string
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "main" {
			return true
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if cc, ok := n.(*ast.CaseClause); ok {
				for _, e := range cc.List {
					if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
						s, _ := strconv.Unquote(lit.Value)
						if !strings.HasPrefix(s, "-") {
							cases = append(cases, s)
						}
					}
				}
			}
			return true
		})
		return false
	})
	slices.Sort(cases)
	want := slices.Clone(topLevelCommands)
	slices.Sort(want)
	if !slices.Equal(cases, want) {
		t.Fatalf("main() dispatches %v; topLevelCommands lists %v", cases, want)
	}
}

// A command used wrongly — an unknown flag, a missing argument — says so in
// one `opod:` line and exits 2, never Go's "Usage of …" (CONTRIBUTING.md
// "CLI output", rule 3).
func TestUsageErrorsExitTwoWithOneLine(t *testing.T) {
	cases := [][]string{
		{"up", "--bogus"}, {"down", "--bogus"}, {"status", "--bogus"}, {"join"}, {"join", "http://x", "--bogus"},
		{"node", "show"}, {"node", "frob"}, {"model", "frob"}, {"shard", "frob"},
		{"catalog", "frob"}, {"catalog", "export"}, {"fetch"}, {"fetch", "--bogus"}, {"batch", "frob"}, {"batch", "run"},
		{"image", "ls", "--bogus"}, {"cache", "ls", "--bogus"}, {"cache", "kv-prune"}, {"token", "frob"},
		{"config", "bogus"}, {"config", "show", "--bogus"}, {"doctor", "--bogus"}, {"update", "--bogus"},
		{"connect", "a", "b"}, {"connect", "--retries", "9"}, {"disconnect", "--bogus"}, {"completion"},
		{"completion", "tcsh"}, {"version", "--bogus"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			got := runTranscript(t, transcriptCase{name: "usage", args: args})
			head, stderr, _ := strings.Cut(got, "--- stderr\n")
			if !strings.Contains(head, "--- exit 2\n") {
				t.Errorf("opod %s: want exit 2\n%s", strings.Join(args, " "), got)
			}
			if !strings.HasPrefix(stderr, "opod: ") || strings.Contains(stderr, "Usage of") {
				t.Errorf("opod %s: want one opod: line on stderr, no Go usage\n%s", strings.Join(args, " "), got)
			}
		})
	}
}

// T24.79: doctor --json is the whole report for a script — every check with
// its state, and the problem count the exit code follows.
func TestDoctorJSONIsTheWholeReport(t *testing.T) {
	got := runTranscript(t, transcriptCase{name: "doctor-json", args: []string{"doctor", "--json"},
		env: map[string]string{"PATH": "/usr/bin:/bin", "OPOD_ENGINE": "bogus", "OPOD_LISTEN": "{{FREE}}"}})
	head, _, _ := strings.Cut(got, "--- stderr\n")
	_, stdout, _ := strings.Cut(head, "--- stdout\n")
	var rep struct {
		Checks   []doctorCheck `json:"checks"`
		Problems int           `json:"problems"`
	}
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}
	names := map[string]string{}
	for _, c := range rep.Checks {
		names[c.Name] = c.State
	}
	for _, want := range []string{"hardware", "config", "leader", "listen port", "admin key", "engine", "data dir", "catalog"} {
		if _, ok := names[want]; !ok {
			t.Errorf("no %q check in %v", want, names)
		}
	}
	if names["engine"] != "problem" || rep.Problems < 1 || !strings.Contains(got, "--- exit 1\n") {
		t.Errorf("a bad engine is a problem and exits 1: %+v\n%s", rep, got)
	}
}
