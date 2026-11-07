package pkg_test

// The wire types moved in from opod-sdk (PLAN T9.50, ADR-118). The control
// plane imports these packages, so what they import it compiles: they may use
// the standard library and each other, nothing else — never a third-party
// module (the CP would build it), never internal/ (that is core's runtime, and
// the CP must not reach it).

import (
	"os/exec"
	"strings"
	"testing"
)

func TestPkgImportsOnlyTheStandardLibraryAndItself(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", "./...").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	for _, p := range strings.Fields(string(out)) {
		if !strings.HasPrefix(p, "github.com/opod-io/opod-core/pkg/") && p != "github.com/opod-io/opod-core/pkg" {
			t.Errorf("a pkg/ package depends on %s: only the standard library and pkg/ itself are allowed", p)
		}
	}
}
