package images

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The load this script used to perform — `curl -X POST .../v1/model/load` with
// a bearer token, and the load_body() that decided between a cached path and a
// repo+file fetch — is now the worker's own work (T10.7): `opod join` calls
// agent.SelfLoad, which cannot be refused by OPOD_REJECT_BEARER because it
// does not authenticate at all. The pinned-version property that was tested
// here against the shell moved with it, to
// internal/agent/selfload_test.go:TestLoadBodyTakesTheCachedFileByPathUnlessTheVersionIsPinned.

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

// A GATEWAY holds no weights (T11.1): it routes to the leader's workers and
// loads nothing, so nothing mounts a models volume for it — and the image root
// is read-only. Every door of the first cell run crash-looped, TWICE for the
// same reason at two depths: the entrypoint made /data/models itself
// (`mkdir: cannot create directory '/data': Permission denied`), and then core
// made its CONFIGURED models directory, whatever the role. So the answer is not
// "make none" — the container does not get that choice — it is "default to
// somewhere the door can write" (2026-09-21).
//
// The script is run for real against a read-only /data, which is the condition
// that produced the failure; asserting the shape of the `if` would not have.
func TestAGatewaysModelsDirectoryIsOneItCanMake(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	root := t.TempDir()
	data := filepath.Join(root, "var")
	// A directory nothing may create under: /data's stand-in.
	ro := filepath.Join(root, "ro")
	if err := os.MkdirAll(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })

	// Up to the role branch only: the real thing would exec `opod up`.
	script := `sed -n '1,/^log() {/p' entrypoint.sh | sed '$d' > "$TMP/head.sh"; bash "$TMP/head.sh" && echo PREPARED`
	// models = "" leaves OPOD_MODELS_DIR unset, which is how a pod runs: the
	// container decides the default, per role.
	runRole := func(role, models string) (string, error) {
		cmd := exec.Command(bash, "-c", script)
		cmd.Dir = "."
		env := []string{"PATH=" + os.Getenv("PATH"), "TMP=" + root,
			"OPOD_ROLE=" + role, "OPOD_DATA_DIR=" + data,
			"OPOD_CATALOG_DIR=" + filepath.Join(root, "catalog"), "HOME=" + data}
		if models != "" {
			env = append(env, "OPOD_MODELS_DIR="+models)
		}
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	// A door's models directory must be one it CAN make: core creates its
	// configured storage directory whatever the role, so "make none" is not a
	// choice the container has — only "somewhere writable" is.
	out, err := runRole("gateway", "")
	if err != nil || !strings.Contains(out, "PREPARED") {
		t.Fatalf("a gateway must prepare without touching the read-only path: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(ro, "models")); err == nil {
		t.Error("a gateway made its models directory under the read-only path")
	}
	if _, err := os.Stat(filepath.Join(data, "models")); err != nil {
		t.Errorf("a gateway's models directory must exist under its writable data dir (core makes the configured one): %v", err)
	}
	// An explicit OPOD_MODELS_DIR is still obeyed for a door: the container
	// picks a DEFAULT, it does not override an operator.
	if out, err := runRole("gateway", filepath.Join(ro, "models")); err == nil && strings.Contains(out, "PREPARED") {
		t.Errorf("an explicit models dir is obeyed, so an unwritable one must fail: %s", out)
	}
	// And a leader's default is unchanged — the shared /data volume it is given.
	if out, err := runRole("leader", filepath.Join(ro, "models")); err == nil && strings.Contains(out, "PREPARED") {
		t.Errorf("a leader with an unwritable models dir must fail, not pass: %s", out)
	}
}
