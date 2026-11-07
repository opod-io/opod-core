package adminapi

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// declaredNames parses env.go and returns the string constants whose
// identifier starts with prefix, as identifier → value. Parsing the source is
// the only way to enumerate constants; it keeps "a constant nobody put in the
// table" a failing test instead of a review comment.
func declaredNames(t *testing.T, prefix string) map[string]string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "env.go", nil, 0)
	if err != nil {
		t.Fatalf("parse env.go: %v", err)
	}
	out := map[string]string{}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, s := range gd.Specs {
			vs := s.(*ast.ValueSpec)
			for i, id := range vs.Names {
				if !strings.HasPrefix(id.Name, prefix) || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("%s must be a string literal", id.Name)
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("%s: %v", id.Name, err)
				}
				out[id.Name] = v
			}
		}
	}
	return out
}

func TestEnvNamesAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, v := range Env() {
		if seen[v.Name] {
			t.Errorf("%s is listed twice", v.Name)
		}
		seen[v.Name] = true
	}
	if len(seen) == 0 {
		t.Fatal("the environment contract is empty")
	}
}

func TestEveryEnvConstantIsARowAndEveryRowHasAConstant(t *testing.T) {
	consts := declaredNames(t, "Env")
	byValue := map[string]string{}
	for id, name := range consts {
		if other, dup := byValue[name]; dup {
			t.Errorf("%s and %s are both %q", id, other, name)
		}
		byValue[name] = id
		if _, ok := Lookup(name); !ok {
			t.Errorf("constant %s = %q is not a row of Env()", id, name)
		}
	}
	for _, v := range Env() {
		if _, ok := byValue[v.Name]; !ok {
			t.Errorf("row %s has no Env* constant", v.Name)
		}
	}
	if len(consts) != len(Env()) {
		t.Errorf("%d constants, %d rows", len(consts), len(Env()))
	}
}

func TestEveryRetiredConstantIsARowAndEveryRowHasAConstant(t *testing.T) {
	consts := declaredNames(t, "RetiredEnv")
	byValue := map[string]bool{}
	for id, name := range consts {
		byValue[name] = true
		if _, ok := LookupRetired(name); !ok {
			t.Errorf("constant %s = %q is not a row of Retired()", id, name)
		}
	}
	for _, v := range Retired() {
		if !byValue[v.Name] {
			t.Errorf("retired row %s has no RetiredEnv* constant", v.Name)
		}
	}
}

var sinceRe = regexp.MustCompile(`^(|[a-z][a-z0-9_]*|v\d+\.\d+\.\d+)$`)

func TestEnvRowsAreWellFormed(t *testing.T) {
	nameRe := regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	for _, v := range Env() {
		if !nameRe.MatchString(v.Name) {
			t.Errorf("%q is not an environment variable name", v.Name)
		}
		switch v.Side {
		case SideLeader, SideWorker, SideBoth:
		default:
			t.Errorf("%s: side %q is not leader | worker | both", v.Name, v.Side)
		}
		if strings.TrimSpace(v.Doc) == "" {
			t.Errorf("%s has no doc", v.Name)
		}
		if !sinceRe.MatchString(v.Since) {
			t.Errorf("%s: since %q is neither empty, a feature key nor an SDK version", v.Name, v.Since)
		}
	}
}

func TestRetiredIsDisjointFromEnvAndComplete(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range Retired() {
		if seen[r.Name] {
			t.Errorf("%s is retired twice", r.Name)
		}
		seen[r.Name] = true
		if _, live := Lookup(r.Name); live {
			t.Errorf("%s is both in the contract and retired", r.Name)
		}
		if !regexp.MustCompile(`^v\d+\.\d+\.\d+$`).MatchString(r.RetiredIn) {
			t.Errorf("%s: retired_in %q is not an SDK version", r.Name, r.RetiredIn)
		}
		if strings.TrimSpace(r.Note) == "" {
			t.Errorf("%s has no note", r.Name)
		}
	}
	// A banner word that no code ever parsed is not a retired variable.
	if _, ok := LookupRetired("OPOD_PROTOCOLS"); ok {
		t.Error("OPOD_PROTOCOLS was never read by any binary; it belongs in no table")
	}
	if _, ok := Lookup("OPOD_PROTOCOLS"); ok {
		t.Error("OPOD_PROTOCOLS was never read by any binary; it belongs in no table")
	}
}

func TestEnvAndRetiredReturnCopies(t *testing.T) {
	a := Env()
	a[0].Name, a[0].Doc = "MUTATED", ""
	a = append(a[:1], a[2:]...)
	_ = a
	if b := Env(); b[0].Name == "MUTATED" || b[0].Doc == "" || b[1].Name != envTable[1].Name {
		t.Fatal("Env() handed out the package's own table")
	}
	if _, ok := Lookup("MUTATED"); ok {
		t.Fatal("Lookup sees a caller's mutation")
	}
	r := Retired()
	r[0].Name = "MUTATED"
	if Retired()[0].Name == "MUTATED" {
		t.Fatal("Retired() handed out the package's own table")
	}
}

func TestLookup(t *testing.T) {
	v, ok := Lookup(EnvLeaderCA)
	if !ok || v.Side != SideWorker || v.Since != "tls_listener" {
		t.Fatalf("Lookup(%s) = %+v, %v", EnvLeaderCA, v, ok)
	}
	if _, ok := Lookup("OPOD_NO_SUCH_VARIABLE"); ok {
		t.Fatal("Lookup found a variable that does not exist")
	}
	if r, ok := LookupRetired(RetiredEnvUI); !ok || r.RetiredIn == "" {
		t.Fatalf("LookupRetired(%s) = %+v, %v", RetiredEnvUI, r, ok)
	}
}
