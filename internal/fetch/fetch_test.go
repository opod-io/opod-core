package fetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeHub serves one file; gets counts GET requests; slow delays the body so
// concurrent callers overlap.
func fakeHub(t *testing.T, body string, slow time.Duration, truncate bool) (*httptest.Server, *int32) {
	t.Helper()
	var gets int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/resolve/main/m.gguf") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", strings.Repeat("", 0)+itoa(len(body)))
		if r.Method == http.MethodHead {
			return
		}
		atomic.AddInt32(&gets, 1)
		time.Sleep(slow)
		if truncate {
			_, _ = w.Write([]byte(body[:len(body)/2]))
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &gets
}

func itoa(n int) string { return strings.TrimSpace(strings.Repeat(" ", 0) + string(rune('0'+n%10))) }

func TestFetchGGUF_ConcurrentCallersPullOnce(t *testing.T) {
	body := "GGUFbody" // 8 bytes: Content-Length "8"
	hub, gets := fakeHub(t, body, 300*time.Millisecond, false)
	dir := t.TempDir()
	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = GGUF(context.Background(), "org/repo", "m.gguf", dir, Options{LockWait: 10 * time.Second, Endpoint: hub.URL})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(gets); got != 1 {
		t.Fatalf("three concurrent callers must pull once, pulled %d times", got)
	}
	b, err := os.ReadFile(filepath.Join(dir, "m.gguf"))
	if err != nil || string(b) != body {
		t.Fatalf("file intact: %q %v", b, err)
	}
	left, _ := filepath.Glob(filepath.Join(dir, "*.partial-*"))
	locks, _ := filepath.Glob(filepath.Join(dir, "*.lock"))
	if len(left) != 0 || len(locks) != 0 {
		t.Fatalf("no temp or lock files left: %v %v", left, locks)
	}
	// Present with the declared size: no second pull.
	if _, err := GGUF(context.Background(), "org/repo", "m.gguf", dir, Options{Endpoint: hub.URL}); err != nil || atomic.LoadInt32(gets) != 1 {
		t.Fatalf("a complete file is not pulled again: %v gets=%d", err, atomic.LoadInt32(gets))
	}
}

func TestFetchGGUF_TruncatedBodyLeavesNoFile(t *testing.T) {
	hub, _ := fakeHub(t, "GGUFbody", 0, true)
	dir := t.TempDir()
	_, err := GGUF(context.Background(), "org/repo", "m.gguf", dir, Options{Endpoint: hub.URL})
	if err == nil || !(strings.Contains(err.Error(), "truncated") || strings.Contains(err.Error(), "unexpected EOF")) {
		t.Fatalf("a short body is refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "m.gguf")); err == nil {
		t.Fatal("no file under the final name after a truncated download")
	}
	left, _ := filepath.Glob(filepath.Join(dir, "*"))
	if len(left) != 0 {
		t.Fatalf("nothing left behind: %v", left)
	}
}

func TestFetchGGUF_WrongSizedFileIsPulledAgain(t *testing.T) {
	hub, gets := fakeHub(t, "GGUFbody", 0, false)
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "m.gguf"), []byte("stale"), 0o644)
	if _, err := GGUF(context.Background(), "org/repo", "m.gguf", dir, Options{Endpoint: hub.URL}); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(gets) != 1 {
		t.Fatal("a file whose size differs from the server's is pulled again")
	}
	if err := ValidGGUFName("../x.gguf"); err == nil {
		t.Fatal("path-y names are refused")
	}
}

// R15.16 · a model VERSION is a pinned Hub revision plus the digest its record
// carries. The properties that make that worth anything:
//   - two revisions of one repo never collide on disk, so an endpoint on v1 and
//     one on v2 can run on the same node;
//   - a file that does not hash to what the record says is not that version,
//     and must not survive to be found "complete" by the next pull.
func TestTwoRevisionsNeverCollide(t *testing.T) {
	dir := t.TempDir()
	a := RevisionDir(dir, "org/model", "abc123")
	b := RevisionDir(dir, "org/model", "def456")
	if a == b {
		t.Fatalf("two revisions share a directory: %s", a)
	}
	if RevisionDir(dir, "org/model", "") != dir {
		t.Fatal("an unpinned fetch must stay at the top of the cache, where every existing file already is")
	}
	if strings.ContainsAny(filepath.Base(a), `/\`) {
		t.Fatalf("the repo slug is not one path segment: %s", a)
	}
}

func TestRevisionIsInTheURL(t *testing.T) {
	got := HFFileURL("", "org/model", "v2.0", "w.gguf")
	if !strings.Contains(got, "/resolve/v2.0/") {
		t.Fatalf("the pinned revision is not in the resolve path: %s", got)
	}
	if !strings.Contains(HFFileURL("", "org/model", "", "w.gguf"), "/resolve/main/") {
		t.Fatal("an empty revision must still resolve main")
	}
}

func TestARevisionCannotEscapeTheCache(t *testing.T) {
	for _, bad := range []string{"../etc", "a/b", `a\b`, "with space"} {
		if err := ValidRevision(bad); err == nil {
			t.Fatalf("revision %q was accepted", bad)
		}
	}
	if err := ValidRevision("a1b2c3d4"); err != nil {
		t.Fatalf("a commit sha was refused: %v", err)
	}
}

func TestAWrongDigestRemovesTheFile(t *testing.T) {
	dir := t.TempDir()
	body := []byte("not the weights you were promised")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	_, err := GGUF(context.Background(), "org/model", "w.gguf", dir, Options{
		Endpoint: srv.URL,
		SHA256:   "0000000000000000000000000000000000000000000000000000000000000000",
	})
	if err == nil {
		t.Fatal("a file that hashed to something else was accepted as the version")
	}
	if !strings.Contains(err.Error(), "hashes to") {
		t.Fatalf("the error does not say what went wrong: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "w.gguf")); statErr == nil {
		t.Fatal("the wrong file survived: the next pull would find it complete and serve it")
	}
}

func TestTheRightDigestPasses(t *testing.T) {
	dir := t.TempDir()
	body := []byte("the weights")
	sum := sha256.Sum256(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	p, err := GGUF(context.Background(), "org/model", "w.gguf", dir, Options{
		Endpoint: srv.URL, Revision: "v1", SHA256: "sha256:" + hex.EncodeToString(sum[:]),
	})
	if err != nil {
		t.Fatalf("a matching digest was refused: %v", err)
	}
	if !strings.Contains(p, "@v1") {
		t.Fatalf("a pinned revision was not cached under its own directory: %s", p)
	}
}
