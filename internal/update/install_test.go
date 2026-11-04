package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRelease serves opod-<platform>.tar.gz holding an `opod` file, and
// checksums.txt only when withSums.
func fakeRelease(t *testing.T, platform string, withSums bool) *httptest.Server {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("#!/bin/sh\necho new\n")
	if err := tw.WriteHeader(&tar.Header{Name: "opod", Mode: 0o755, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write(body)
	_ = tw.Close()
	_ = gz.Close()
	archive := buf.Bytes()
	sum := sha256.Sum256(archive)
	asset := "opod-" + platform + ".tar.gz"
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/"+asset):
			_, _ = w.Write(archive)
		case strings.HasSuffix(r.URL.Path, "/checksums.txt") && withSums:
			_, _ = w.Write([]byte(hex.EncodeToString(sum[:]) + "  " + asset + "\n"))
		default:
			http.NotFound(w, r)
		}
	}))
}

// PLAN T10.20: a release with no checksums.txt is refused with a message
// that says why and what to do, and the installed binary is not touched.
func TestInstallRefusesAReleaseWithNoChecksums(t *testing.T) {
	srv := fakeRelease(t, "linux-amd64", false)
	defer srv.Close()
	target := filepath.Join(t.TempDir(), "opod")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := installFrom(srv.URL+"/v9.9.9", "v9.9.9", "linux-amd64", target, nil)
	if err == nil || !strings.Contains(err.Error(), "refusing to install a binary nobody can verify") {
		t.Fatalf("a release with no checksums.txt: %v", err)
	}
	if b, _ := os.ReadFile(target); string(b) != "old" {
		t.Fatalf("the installed binary changed: %q", b)
	}
}

// The same release WITH a matching checksums.txt installs.
func TestInstallVerifiesAndInstalls(t *testing.T) {
	srv := fakeRelease(t, "linux-amd64", true)
	defer srv.Close()
	target := filepath.Join(t.TempDir(), "opod")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	var lines []string
	if err := installFrom(srv.URL+"/v9.9.9", "v9.9.9", "linux-amd64", target, func(f string, a ...any) { lines = append(lines, f) }); err != nil {
		t.Fatalf("a verified release: %v", err)
	}
	if b, _ := os.ReadFile(target); !strings.Contains(string(b), "echo new") {
		t.Fatalf("not installed: %q", b)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "checksum verified") {
		t.Fatalf("progress: %v", lines)
	}
}
