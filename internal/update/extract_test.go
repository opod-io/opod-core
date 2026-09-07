package update

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

// writeTarGz builds a .tar.gz at path from the given entries.
func writeTarGz(t *testing.T, path string, entries []*tar.Header, bodies [][]byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for i, h := range entries {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if len(bodies[i]) > 0 {
			if _, err := tw.Write(bodies[i]); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestExtractTarGz_RejectsTraversalAndSymlinks(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "evil.tar.gz")
	out := filepath.Join(dir, "out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}

	entries := []*tar.Header{
		{Name: "opod", Typeflag: tar.TypeReg, Mode: 0o755, Size: 3},                     // legit
		{Name: "../escape.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4},            // traversal
		{Name: "/abs.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4},                 // absolute
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd", Mode: 0o777}, // symlink
	}
	bodies := [][]byte{[]byte("bin"), []byte("pwn!"), []byte("pwn!"), nil}
	writeTarGz(t, src, entries, bodies)

	if err := ExtractTarGz(src, out); err != nil {
		t.Fatalf("extract: %v", err)
	}

	// The legit binary must be there.
	if _, err := os.Stat(filepath.Join(out, "opod")); err != nil {
		t.Fatalf("expected opod binary extracted: %v", err)
	}
	// The traversal entry must NOT have escaped the output dir.
	if _, err := os.Stat(filepath.Join(dir, "escape.txt")); err == nil {
		t.Fatal("path-traversal entry escaped the extraction dir")
	}
	// The symlink must NOT have been created (as link or as a file).
	if _, err := os.Lstat(filepath.Join(out, "link")); err == nil {
		t.Fatal("symlink entry was extracted; expected it to be skipped")
	}
}

func TestLookupChecksum_NormalizesNamePrefixes(t *testing.T) {
	dir := t.TempDir()
	sums := filepath.Join(dir, "checksums.txt")
	// Mix of plain "  name", binary "*name", and "./name" forms.
	content := "aaa  opod-darwin-arm64.tar.gz\n" +
		"bbb *opod-linux-amd64.tar.gz\n" +
		"ccc  ./opod-linux-arm64.tar.gz\n"
	if err := os.WriteFile(sums, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"opod-darwin-arm64.tar.gz": "aaa",
		"opod-linux-amd64.tar.gz":  "bbb",
		"opod-linux-arm64.tar.gz":  "ccc",
	}
	for artifact, want := range cases {
		got, ok := LookupChecksum(sums, artifact)
		if !ok || got != want {
			t.Errorf("LookupChecksum(%q) = %q,%v; want %q,true", artifact, got, ok, want)
		}
	}
}
