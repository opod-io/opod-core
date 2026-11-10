package main

import (
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
