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
	"testing"
)

// The digest is taken from the download stream, not from a second read of the
// file (PLAN T15.7): a wrong digest never reaches the final name — not even
// for the instant between rename and the old verify — and leaves no partial
// behind; a right one is served by ONE GET of the body.
func TestDigestIsCheckedOnTheStream(t *testing.T) {
	body := []byte("the weights, hashed as they land")
	sum := sha256.Sum256(body)
	gets := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		if r.Method == http.MethodHead {
			return
		}
		gets++
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	dir := t.TempDir()
	_, err := GGUF(context.Background(), "org/model", "w.gguf", dir, Options{
		Endpoint: srv.URL, Revision: "v1", SHA256: strings.Repeat("0", 64),
	})
	if err == nil || !strings.Contains(err.Error(), "hashes to") {
		t.Fatalf("a wrong digest must be refused by name: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "org", "model", "v1"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "w.gguf") {
			t.Fatalf("a wrong file left %q behind", e.Name())
		}
	}

	p, err := GGUF(context.Background(), "org/model", "w.gguf", dir, Options{
		Endpoint: srv.URL, Revision: "v1", SHA256: "sha256:" + hex.EncodeToString(sum[:]),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p); string(got) != string(body) {
		t.Fatal("the right file is not on disk whole")
	}
	if gets != 2 {
		t.Fatalf("two pulls = two GETs, got %d", gets)
	}
}
