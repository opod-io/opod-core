package main

// Images are described in three places — images/build.sh (the local lane),
// .github/workflows/images.yml (the release lane) and images/README.md (what a
// reader is told) — and on 2026-09-15 all three had drifted at once: the release
// lane built the AMD and CPU workers from a Dockerfile with no RPC pair while
// build.sh used the right one, and an SGLang row named a base tag Docker Hub has
// never had. Each cost a full run to discover.
//
// These tests are the tripwire: one image is one row in every list, built from
// the same Dockerfile on the same base, and the llama.cpp release is one string.
// They read the files rather than a generated manifest, because the files are
// what the two lanes actually execute.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func repoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// buildSpec is one image as images/build.sh's spec() describes it.
type buildSpec struct{ name, file, base string }

var specRe = regexp.MustCompile(`(?m)^\s+([a-z0-9-]+)\)\s+echo "(opod-[a-z0-9-]+) (images/[^ ]+) ([^ ]+) `)

func buildSpecs(t *testing.T) map[string]buildSpec {
	t.Helper()
	sh := repoFile(t, "images/build.sh")
	// Resolve the ${VAR:-default} bases build.sh keeps at the top, so a row that
	// names a variable is compared by the value it actually builds with.
	vars := map[string]string{}
	for _, m := range regexp.MustCompile(`(?m)^([A-Z_]+)=\$\{[A-Z_]+:-([^}]+)\}`).FindAllStringSubmatch(sh, -1) {
		vars[m[1]] = m[2]
	}
	out := map[string]buildSpec{}
	for _, m := range specRe.FindAllStringSubmatch(sh, -1) {
		base := m[4]
		if strings.HasPrefix(base, "$") {
			if v, ok := vars[strings.Trim(base, "${}")]; ok {
				base = v
			}
		}
		out[m[2]] = buildSpec{name: m[2], file: m[3], base: base}
	}
	if len(out) < 8 {
		t.Fatalf("parsed only %d rows from images/build.sh — the spec() shape changed, fix this test", len(out))
	}
	return out
}

// TestReleaseLaneMatchesBuildScript: every image the release workflow builds is a
// row in build.sh, from the same Dockerfile and the same base. A mismatch here is
// a customer release built differently from what we test locally.
func TestReleaseLaneMatchesBuildScript(t *testing.T) {
	specs := buildSpecs(t)
	wf := repoFile(t, ".github/workflows/images.yml")
	rowRe := regexp.MustCompile(`- \{ name: (opod-[a-z0-9-]+),\s+file: (images/[^,]+),\s+base: "?([^",]*)"?,`)
	rows := rowRe.FindAllStringSubmatch(wf, -1)
	if len(rows) < 8 {
		t.Fatalf("parsed only %d matrix rows from images.yml — the matrix shape changed, fix this test", len(rows))
	}
	for _, r := range rows {
		name, file, base := r[1], strings.TrimSpace(r[2]), strings.TrimSpace(r[3])
		spec, ok := specs[name]
		if !ok {
			if name == "opod-leader" { // the leader has no engine/vendor row in spec()
				continue
			}
			t.Errorf("%s is built by the release lane but has no images/build.sh row: add one, or nobody can build it locally", name)
			continue
		}
		if spec.file != file {
			t.Errorf("%s: release lane builds %s, build.sh builds %s — one of them ships something we never test", name, file, spec.file)
		}
		if base != "" && spec.base != base {
			t.Errorf("%s: release lane base %s, build.sh base %s", name, base, spec.base)
		}
	}
}

// TestEveryImageIsDocumented: the README table is what a reader (and the chart's
// author) goes by, so an image missing from it is an image nobody knows to pin.
func TestEveryImageIsDocumented(t *testing.T) {
	readme := repoFile(t, "images/README.md")
	for name := range buildSpecs(t) {
		if !strings.Contains(readme, "`"+name+"`") {
			t.Errorf("%s is built but not in the images/README.md table", name)
		}
	}
}

// TestDockerfilesReferencedExist: a row naming a file that is not there fails a
// run minutes in; here it fails in milliseconds.
func TestDockerfilesReferencedExist(t *testing.T) {
	for name, s := range buildSpecs(t) {
		if _, err := os.Stat(filepath.Join("..", "..", s.file)); err != nil {
			t.Errorf("%s names %s, which does not exist", name, s.file)
		}
	}
}

// TestOneLlamaRelease: the release is an ARG default in every llama.cpp recipe and
// build.sh reads it from one of them, so they must agree — otherwise an image
// carries binaries from a different llama.cpp than its tag claims.
func TestOneLlamaRelease(t *testing.T) {
	dir := filepath.Join("..", "..", "images", "worker-llamacpp")
	files, err := filepath.Glob(filepath.Join(dir, "Dockerfile*"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no llama.cpp Dockerfiles found: %v", err)
	}
	re := regexp.MustCompile(`(?m)^ARG LLAMA_RELEASE=(\S+)`)
	seen := map[string][]string{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		m := re.FindStringSubmatch(string(b))
		if m == nil {
			t.Errorf("%s has no ARG LLAMA_RELEASE default", filepath.Base(f))
			continue
		}
		seen[m[1]] = append(seen[m[1]], filepath.Base(f))
	}
	if len(seen) > 1 {
		t.Errorf("llama.cpp recipes disagree on LLAMA_RELEASE: %v", seen)
	}
}

// TestPrebuiltPairsAreSubstituted: a recipe that compiles an RPC pair must also
// export it (target rpc-export) and be wired into build.sh's rpc_vendor_of, or the
// compile silently happens inside every image build — under emulation, for an hour.
func TestPrebuiltPairsAreSubstituted(t *testing.T) {
	sh := repoFile(t, "images/build.sh")
	for _, vendor := range []string{"cuda", "sycl", "cpu"} {
		f := filepath.Join("..", "..", "images", "worker-llamacpp", "Dockerfile.rpc-"+vendor)
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if !strings.Contains(string(b), "AS rpc-export") {
			t.Errorf("Dockerfile.rpc-%s compiles a pair but has no rpc-export target to publish it", vendor)
		}
		if !strings.Contains(sh, "echo "+vendor+" ;;") {
			t.Errorf("Dockerfile.rpc-%s is not wired into build.sh rpc_vendor_of, so its pair is never substituted", vendor)
		}
	}
}
