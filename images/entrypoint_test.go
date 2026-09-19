package images

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// loadBody runs the entrypoint's load_body with the given environment.
func loadBody(t *testing.T, models string, env ...string) string {
	t.Helper()
	script := `eval "$(sed -n '/^load_body() {/,/^}/p' entrypoint.sh)"; load_body m org/repo w.gguf "$MODELS"`
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "MODELS=" + models}, env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("load_body: %v\n%s", err, out)
	}
	return string(out)
}

// A pinned model version must never be satisfied by a file that merely has the
// same name. The entrypoint took "the whole file at the top of the node cache"
// by path whenever one existed — which skipped the revision directory and the
// digest check, so a node that had once served the model unpinned served those
// bytes under every pinned version (found by a cluster scenario that rolls
// v1 → v2 → back: nothing was ever cached under <repo>@<revision>).
func TestAPinnedModelNeverTakesTheUnpinnedFileByPath(t *testing.T) {
	models := t.TempDir()
	if err := os.WriteFile(filepath.Join(models, "w.gguf"), []byte("unpinned bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	byPath := `{"id":"m","path":"` + models + `/w.gguf"}`
	byRepo := `{"id":"m","repo":"org/repo","file":"w.gguf"}`

	if got := loadBody(t, models); got != byPath {
		t.Fatalf("unpinned, file cached: want the path (no second download), got %s", got)
	}
	for _, pin := range []string{"OPOD_MODEL_REVISION=2084ee2", "OPOD_MODEL_SHA256=abc"} {
		if got := loadBody(t, models, pin); got != byRepo {
			t.Fatalf("%s: want the agent to fetch and verify (%s), got %s", strings.SplitN(pin, "=", 2)[0], byRepo, got)
		}
	}
	if got := loadBody(t, t.TempDir()); got != byRepo {
		t.Fatalf("unpinned, nothing cached: want repo+file, got %s", got)
	}
}
