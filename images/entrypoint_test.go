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

// baseEnv runs the entrypoint's base_env against a record file and prints what
// the rest of the entrypoint would then see.
func baseEnv(t *testing.T, record string) (string, error) {
	t.Helper()
	script := `set -euo pipefail; eval "$(sed -n '/^base_env() {/,/^}/p' entrypoint.sh)"; base_env; echo "lib=${BASE_LIB:-} strict=$-"`
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "OPOD_BASE_ENV_FILE=" + record}
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// A base image that prepared its environment in its own ENTRYPOINT (Intel's XPU
// vLLM image sources oneAPI's setvars.sh) lost it under ours: the engine died at
// `import torch` with "libccl.so.1: cannot open shared object file". The image
// now records the script; the entrypoint sources it — with its arguments, and
// although such scripts read unset variables and return non-zero freely.
func TestTheBaseImagesEnvironmentScriptIsSourced(t *testing.T) {
	dir := t.TempDir()
	vendor := filepath.Join(dir, "setvars.sh")
	body := "echo noise\n[ \"$1\" = --force ] || return 3\n: \"$NEVER_SET\"\nfalse\nexport BASE_LIB=/opt/vendor/lib\n"
	if err := os.WriteFile(vendor, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(dir, "base-env")
	if err := os.WriteFile(record, []byte(vendor+" --force\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := baseEnv(t, record)
	if err != nil || !strings.HasPrefix(got, "lib=/opt/vendor/lib strict=") || !strings.Contains(got, "e") || !strings.Contains(got, "u") {
		t.Fatalf("want the script's exports, none of its output, and -eu back on afterwards; got %q (%v)", got, err)
	}
	if got, err := baseEnv(t, filepath.Join(dir, "absent")); err != nil || !strings.HasPrefix(got, "lib= ") {
		t.Fatalf("an image that records nothing starts as before; got %q (%v)", got, err)
	}
	if err := os.WriteFile(record, []byte(filepath.Join(dir, "gone.sh")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := baseEnv(t, record); err == nil || !strings.Contains(got, "does not have") {
		t.Fatalf("a record naming a script the image lacks must stop the start, by name; got %q (%v)", got, err)
	}
}
