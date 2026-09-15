package fetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
