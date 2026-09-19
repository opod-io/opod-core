package config

// The `ui`, `egress` and `callbacks` switches were removed because the surfaces
// they switched left the binary; these tests hold the promise that removing
// them broke nobody who still sets them.

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// isolateEnv points the process at an empty home so Load sees only what the
// test writes, and clears every switch a developer's shell might carry.
func isolateEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	for _, v := range []string{"OPOD_UI", "OPOD_EGRESS", "OPOD_CALLBACKS", "OPOD_PROTOCOLS", "OPOD_MANAGED", "OPOD_DATA_DIR"} {
		t.Setenv(v, "")
	}
}

// Every config.yaml an older binary saved carries the three keys (Save wrote
// the whole struct), and a manager may template them. Such a file must load.
func TestAConfigWithTheRetiredSurfaceKeysStillLoads(t *testing.T) {
	isolateEnv(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "listen: \":9090\"\nsurfaces:\n  ui: false\n  egress: false\n  callbacks: false\n  managed: true\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("a config that loaded yesterday must load today: %v", err)
	}
	if cfg.Listen != ":9090" || !cfg.Surfaces.Managed {
		t.Fatalf("the keys that still mean something were lost: listen=%q managed=%v", cfg.Listen, cfg.Surfaces.Managed)
	}
}

// A manager's pod template sets OPOD_UI=off OPOD_EGRESS=off OPOD_CALLBACKS=off.
// Nothing reads them any more, so the config they produce is the config their
// absence produces.
func TestTheRetiredSurfaceVariablesChangeNothing(t *testing.T) {
	isolateEnv(t)
	missing := filepath.Join(t.TempDir(), "absent.yaml")
	plain, err := Load(missing)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPOD_UI", "off")
	t.Setenv("OPOD_EGRESS", "off")
	t.Setenv("OPOD_CALLBACKS", "off")
	t.Setenv("OPOD_PROTOCOLS", "openai")
	with, err := Load(missing)
	if err != nil {
		t.Fatalf("a retired variable made loading fail: %v", err)
	}
	if !reflect.DeepEqual(plain, with) {
		t.Fatalf("a retired variable changed the config:\n without: %+v\n with:    %+v", plain, with)
	}
}

// They are not part of the environment contract either: a manager reading
// Vars() to know what to render is not told to set variables that do nothing.
func TestTheRetiredSurfaceVariablesAreNotInTheContract(t *testing.T) {
	for _, v := range Vars() {
		switch v.Name {
		case "OPOD_UI", "OPOD_EGRESS", "OPOD_CALLBACKS", "OPOD_PROTOCOLS":
			t.Errorf("%s is in the environment contract but nothing reads it", v.Name)
		}
	}
}

func TestSummaryNamesOnlyTheModeThatExists(t *testing.T) {
	for _, c := range []struct {
		s    SurfacesConfig
		want string
	}{{SurfacesConfig{}, "standalone"}, {SurfacesConfig{Managed: true}, "managed"}} {
		got := c.s.Summary()
		if got != c.want {
			t.Errorf("Summary(%+v) = %q, want %q", c.s, got, c.want)
		}
		for _, gone := range []string{"ui", "egress", "callbacks"} {
			if strings.Contains(got, gone) {
				t.Errorf("Summary(%+v) = %q names %q, a surface this binary does not have", c.s, got, gone)
			}
		}
	}
}
