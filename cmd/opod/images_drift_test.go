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

	"github.com/opod-io/opod/images"
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
type buildSpec struct{ name, file, base, arches string }

var specRe = regexp.MustCompile(`(?m)^\s+([a-z0-9-]+)\)\s+echo "(opod-[a-z0-9-]+) (images/[^ ]+) ([^ ]+) ([a-z0-9,]+)"`)

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
		out[m[2]] = buildSpec{name: m[2], file: m[3], base: base, arches: m[5]}
	}
	if len(out) < 8 {
		t.Fatalf("parsed only %d rows from images/build.sh — the spec() shape changed, fix this test", len(out))
	}
	return out
}

// TestManifestMatchesBuildScript: images/images.yaml is what `opod image` prints
// and what an agent plans a deployment from, so it may only describe images the
// lanes really build: the same set, from the same Dockerfile, on the same base
// (tag; the digest stays with the lanes) for the same platforms. And an image
// that claims gang=rpc must be one whose Dockerfile sets the RPC label — the
// claim is what sends a gang part to it.
func TestManifestMatchesBuildScript(t *testing.T) {
	specs := buildSpecs(t)
	manifest, err := images.Load()
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, img := range manifest.Images {
		listed[img.Name] = true
		spec, ok := specs[img.Name]
		if !ok {
			t.Errorf("%s is in images/images.yaml but has no images/build.sh row: the CLI would offer an image nobody builds", img.Name)
			continue
		}
		if img.Dockerfile != spec.file {
			t.Errorf("%s: manifest says %s, build.sh builds %s", img.Name, img.Dockerfile, spec.file)
		}
		if got := strings.Join(img.Arch, ","); got != spec.arches {
			t.Errorf("%s: manifest platforms %s, build.sh %s", img.Name, got, spec.arches)
		}
		// The leader's row has no BASE argument ("-"); its base is the Dockerfile's FROM.
		if tag, _, _ := strings.Cut(spec.base, "@"); spec.base != "-" && img.Base != tag {
			t.Errorf("%s: manifest base %s, build.sh base %s", img.Name, img.Base, tag)
		}
		labelled := strings.Contains(repoFile(t, img.Dockerfile), `LABEL io.opod.llamacpp.rpc="true"`)
		if claims := img.Gang == images.GangRPC; claims != labelled {
			t.Errorf("%s: manifest gang=%q but %s sets the RPC label: %v", img.Name, img.Gang, img.Dockerfile, labelled)
		}
	}
	for name := range specs {
		if !listed[name] {
			t.Errorf("%s is built but missing from images/images.yaml, so `opod image ls` never shows it", name)
		}
	}
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
	for _, vendor := range []string{"cuda", "sycl", "cpu", "rocm"} {
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

// digestPinned is a base that cannot move under us: <ref>[:tag]@sha256:<64 hex>.
var digestPinned = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)

// TestEveryExternalBaseIsDigestPinned: a tag is a name upstream may repoint at any
// time (`full-cuda` moves with every llama.cpp release), and an image built FROM
// one is not reproducible from the commit its tag names. Every FROM that leaves
// the Dockerfile — and every BASE either lane passes in over the ARG default —
// names a digest. What stays unpinned on purpose: `scratch`, a stage of the same
// file, and a build context the lanes substitute (the prebuilt opod binary and
// the prebuilt RPC pairs are our own artefacts, addressed by release).
// Moving a pin is `images/build.sh refresh-bases`, in a commit of its own.
func TestEveryExternalBaseIsDigestPinned(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "images", "*", "Dockerfile*"))
	if err != nil || len(files) < 8 {
		t.Fatalf("found %d Dockerfiles under images/ (%v) — the layout changed, fix this test", len(files), err)
	}
	argRe := regexp.MustCompile(`(?m)^ARG\s+([A-Za-z_][A-Za-z0-9_]*)=(\S+)`)
	fromRe := regexp.MustCompile(`(?mi)^FROM\s+(?:--platform=\S+\s+)?(\S+)(?:\s+AS\s+(\S+))?`)
	copyFromRe := regexp.MustCompile(`(?m)^COPY\s+(?:--\S+\s+)*--from=(\S+)`)
	varRe := regexp.MustCompile(`\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?`)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		src, name := string(b), filepath.Base(filepath.Dir(f))+"/"+filepath.Base(f)
		args := map[string]string{}
		for _, m := range argRe.FindAllStringSubmatch(src, -1) {
			args[m[1]] = m[2]
		}
		stages := map[string]bool{"scratch": true}
		froms := fromRe.FindAllStringSubmatch(src, -1)
		if len(froms) == 0 {
			t.Errorf("%s: no FROM line parsed", name)
		}
		for _, m := range froms {
			ref := varRe.ReplaceAllStringFunc(m[1], func(v string) string {
				return args[varRe.FindStringSubmatch(v)[1]]
			})
			if !stages[ref] && !digestPinned.MatchString(ref) {
				t.Errorf("%s: FROM %s resolves to %q, which is not pinned by digest — pin it as <ref>:<tag>@sha256:… (images/build.sh refresh-bases keeps it current)", name, m[1], ref)
			}
			if m[2] != "" {
				stages[m[2]] = true
			}
		}
		for _, m := range copyFromRe.FindAllStringSubmatch(src, -1) {
			if !stages[m[1]] && !digestPinned.MatchString(m[1]) {
				t.Errorf("%s: COPY --from=%s is neither a stage of this file nor a digest-pinned image", name, m[1])
			}
		}
	}

	// The lanes override ARG BASE, so the value they pass is the one that ships.
	for name, s := range buildSpecs(t) {
		if s.base != "-" && !digestPinned.MatchString(s.base) {
			t.Errorf("%s: images/build.sh builds it on %q, which is not pinned by digest", name, s.base)
		}
	}
	wf := repoFile(t, ".github/workflows/images.yml")
	for _, r := range regexp.MustCompile(`- \{ name: (opod-[a-z0-9-]+),[^}]*\bbase: "?([^",]*)"?,`).FindAllStringSubmatch(wf, -1) {
		if base := strings.TrimSpace(r[2]); base != "" && !digestPinned.MatchString(base) {
			t.Errorf("%s: the release lane builds it on %q, which is not pinned by digest", r[1], base)
		}
	}
}

// TestOneDigestPerBase: the same upstream tag is named in a Dockerfile's ARG
// default, in build.sh and in the release workflow. If they disagree on its
// digest, the two lanes ship different images under one name.
func TestOneDigestPerBase(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join("..", "..", "images", "*", "Dockerfile*"))
	files = append(files, filepath.Join("..", "..", "images", "build.sh"), filepath.Join("..", "..", ".github", "workflows", "images.yml"))
	pinRe := regexp.MustCompile(`([A-Za-z0-9./_-]+:[A-Za-z0-9._-]+)@(sha256:[0-9a-f]{64})`)
	seen := map[string]map[string][]string{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, m := range pinRe.FindAllStringSubmatch(string(b), -1) {
			ref := strings.TrimPrefix(m[1], "docker.io/")
			if seen[ref] == nil {
				seen[ref] = map[string][]string{}
			}
			seen[ref][m[2]] = append(seen[ref][m[2]], filepath.Base(f))
		}
	}
	if len(seen) < 10 {
		t.Fatalf("found only %d pinned bases — the pin shape changed, fix this test", len(seen))
	}
	for ref, digests := range seen {
		if len(digests) > 1 {
			t.Errorf("%s is pinned to %d different digests: %v — run images/build.sh refresh-bases", ref, len(digests), digests)
		}
	}
}

// rpcLabel is how a llama.cpp worker image says it can be a part of an RPC gang.
const rpcLabel = `LABEL io.opod.llamacpp.rpc="true"`

// TestRPCLabelMatchesThePair: upstream's GPU builds of llama.cpp ship without the
// RPC backend, so "is this a llama.cpp image" does not answer "can it join a
// gang" — a part on an image without rpc-server dies at process launch. The label
// is the image's own statement, read from the registry by whoever schedules it,
// so it must be exactly as true as the Dockerfile: present where the final stage
// carries the pair and the rpc-server entry point, absent everywhere else.
func TestRPCLabelMatchesThePair(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "images", "*", "Dockerfile*"))
	if err != nil || len(files) < 8 {
		t.Fatalf("found %d Dockerfiles under images/ (%v) — the layout changed, fix this test", len(files), err)
	}
	pairCopy := regexp.MustCompile(`(?m)^COPY --from=rpc-[a-z]+ /opt/llama-rpc /opt/llama-rpc$`)
	labelled := 0
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		src, name := string(b), filepath.Base(filepath.Dir(f))+"/"+filepath.Base(f)
		// Only the last stage is the image that ships; rpc-export also copies the
		// pair, but it is a carrier for the binaries, not a worker.
		final := src[strings.LastIndex(src, "\nFROM "):]
		hasPair := pairCopy.MatchString(final) && strings.Contains(final, "> /usr/local/bin/rpc-server")
		hasLabel := strings.Contains(final, rpcLabel)
		switch {
		case hasPair && !hasLabel:
			t.Errorf("%s ships rpc-server but does not say so: add %s to its final stage", name, rpcLabel)
		case hasLabel && !hasPair:
			t.Errorf("%s claims %s but its final stage has no /opt/llama-rpc pair and rpc-server entry point", name, rpcLabel)
		}
		if strings.Count(src, "io.opod.llamacpp.rpc") != strings.Count(final, rpcLabel) {
			t.Errorf("%s names the RPC label outside its final stage, or in another spelling", name)
		}
		if hasLabel {
			labelled++
		}
	}
	if labelled == 0 {
		t.Error("no image carries the RPC label — the pair or the label shape changed, fix this test")
	}
	if !strings.Contains(repoFile(t, "images/README.md"), "io.opod.llamacpp.rpc") {
		t.Error("images/README.md does not document the io.opod.llamacpp.rpc label")
	}
}
