package config

// The process environment is a contract (ROADMAP R12.1 → R12.2): a manager
// that launches opod sets OPOD_* variables, and a variable read somewhere in
// library code is a promise nobody wrote down. This test is the written-down
// list: every environment read outside this package and outside cmd/opod
// (which parses flags and is the one place allowed to touch the environment
// on its way to config) must be here. A new read is a failing test, and the
// fix is to route it through Config — R12.2 shrinks this list to zero, at
// which point the list itself goes.
//
// Entries are "<path from internal/>: <variable>"; a read whose argument is
// not a string literal is recorded as "<path>: <dynamic>".

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

var allowedEnvReads = []string{
	"agent/adapters.go: OPOD_ADAPTERS",
	"agent/capability.go: OPOD_ACCELERATOR",
	"agent/engineflags.go: OPOD_ENGINE_FLAGS",
	"agent/server.go: HF_TOKEN",
	"agent/server.go: OPOD_REJECT_BEARER",
	"agent/server.go: OPOD_SLEEP_MODE",
	"controlplane/authfile.go: OPOD_AUTH_FILE",
	"controlplane/planfile.go: OPOD_PLAN_FILE",
	"controlplane/policyfile.go: OPOD_POLICY_FILE",
	"fetch/fetch.go: HF_ENDPOINT",
	"models/catalog.go: OPOD_CATALOG_DIR",
	"models/probe.go: OPOD_SKIP_SOURCE_CHECK",
	"scheduler/hf_download.go: HF_TOKEN",
	"scheduler/placement.go: OPOD_COORDINATOR_NODE",
	"scheduler/sharding.go: OPOD_COORDINATOR_NODE",
}

func TestEnvSurfaceOutsideConfigIsTheAllowlist(t *testing.T) {
	root := filepath.Join("..") // internal/
	found := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == root {
				return nil
			}
			if filepath.Base(path) == "config" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "os" {
				return true
			}
			switch sel.Sel.Name {
			case "Getenv", "LookupEnv", "Setenv", "Unsetenv":
			default:
				return true
			}
			name := "<dynamic>"
			if len(call.Args) > 0 {
				if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					name, _ = strconv.Unquote(lit.Value)
				}
			}
			found[rel+": "+name] = true
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{}
	for _, a := range allowedEnvReads {
		allowed[a] = true
	}
	var extra, stale []string
	for k := range found {
		if !allowed[k] {
			extra = append(extra, k)
		}
	}
	for k := range allowed {
		if !found[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(extra)
	sort.Strings(stale)
	if len(extra) > 0 {
		t.Errorf("environment reads outside internal/config that are not on the list — route them through Config instead of adding them:\n  %s", strings.Join(extra, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("list entries no longer in the tree — remove them so the list only shrinks:\n  %s", strings.Join(stale, "\n  "))
	}
}
