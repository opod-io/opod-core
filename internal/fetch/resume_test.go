package fetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// rangeHub serves body, honours Range when honour is true, and counts the
// bytes it was asked for so a test can prove a resume did not re-fetch.
func rangeHub(t *testing.T, body string, honour bool, served *int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/api/models/") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"sha":"deadbeef","siblings":[{"rfilename":"m.gguf"}]}`)
			return
		}
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusOK)
			return
		}
		from := 0
		if rng := r.Header.Get("Range"); rng != "" && honour {
			n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rng, "bytes="), "-"))
			if err == nil && n < len(body) {
				from = n
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", n, len(body)-1, len(body)))
				w.Header().Set("Content-Length", strconv.Itoa(len(body)-n))
				w.WriteHeader(http.StatusPartialContent)
			}
		}
		*served += int64(len(body) - from)
		_, _ = w.Write([]byte(body[from:]))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// A 40 GB pull that dies at 39 GB used to start from zero. It now asks for
// the rest: the bytes already on disk are hashed back in, so the single pass
// over the NETWORK is kept and the digest still proves the whole file (T10.9).
func TestAnInterruptedPullResumes(t *testing.T) {
	const body = "0123456789abcdef"
	var served int64
	hub := rangeHub(t, body, true, &served)
	dir := t.TempDir()

	// Half the file is already on disk under the one deterministic name.
	target := filepath.Join(dir, "m.gguf")
	if err := os.WriteFile(partialPath(target), []byte(body[:8]), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := GGUF(context.Background(), "org/repo", "m.gguf", dir,
		Options{Endpoint: hub.URL, SHA256: sum(body)}); err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != body {
		t.Fatalf("file = %q (%v), want %q", got, err, body)
	}
	if served != 8 {
		t.Fatalf("the server was asked for %d bytes, want 8 — the resume re-fetched what we had", served)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*.partial-*")); len(left) != 0 {
		t.Fatalf("partial left behind after success: %v", left)
	}
}

// A server that ignores Range answers 200 with the whole file. That is not a
// failure: start over, and do not mix the old bytes into the hash.
func TestAServerThatIgnoresRangeStartsOver(t *testing.T) {
	const body = "0123456789abcdef"
	var served int64
	hub := rangeHub(t, body, false, &served)
	dir := t.TempDir()
	target := filepath.Join(dir, "m.gguf")
	if err := os.WriteFile(partialPath(target), []byte("xxxxxxxx"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := GGUF(context.Background(), "org/repo", "m.gguf", dir,
		Options{Endpoint: hub.URL, SHA256: sum(body)}); err != nil {
		t.Fatalf("a 200 answer to a Range request must still work: %v", err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != body {
		t.Fatalf("file = %q, want %q — the stale partial was not discarded", got, body)
	}
}

// A partial whose bytes are NOT the file's first bytes resumes, completes the
// length, and is then caught by the digest — and removed, so the next attempt
// does not resume from bytes we know are bad.
func TestACorruptPartialIsCaughtAndRemoved(t *testing.T) {
	const body = "0123456789abcdef"
	var served int64
	hub := rangeHub(t, body, true, &served)
	dir := t.TempDir()
	target := filepath.Join(dir, "m.gguf")
	if err := os.WriteFile(partialPath(target), []byte("XXXXXXXX"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := GGUF(context.Background(), "org/repo", "m.gguf", dir,
		Options{Endpoint: hub.URL, SHA256: sum(body)})
	if err == nil || !strings.Contains(err.Error(), "hashes to") {
		t.Fatalf("a corrupt resume must fail the digest, got %v", err)
	}
	if _, err := os.Stat(partialPath(target)); !os.IsNotExist(err) {
		t.Fatal("the bad partial was kept: every later attempt would resume from it")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("a file that failed its digest reached the final name")
	}
}

func TestParseContentRange(t *testing.T) {
	cases := []struct {
		in         string
		start, tot int64
		ok         bool
	}{
		{"bytes 100-199/200", 100, 200, true},
		{"bytes 0-0/1", 0, 1, true},
		{"bytes 100-199/*", 100, 0, true},
		{"bytes */200", 0, 0, false},
		{"", 0, 0, false},
		{"100-199/200", 100, 200, true},
	}
	for _, tc := range cases {
		start, tot, ok := parseContentRange(tc.in)
		if ok != tc.ok || (ok && (start != tc.start || tot != tc.tot)) {
			t.Fatalf("parseContentRange(%q) = %d,%d,%v want %d,%d,%v", tc.in, start, tot, ok, tc.start, tc.tot, tc.ok)
		}
	}
}
