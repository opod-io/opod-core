package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/opod-io/opod/internal/store"
)

// TestEgressRotatesOn429 proves the core multi-account behavior: when the
// first key gets a 429, the egress layer parks it and transparently retries
// with the user's next key for the same provider — the client only ever sees
// the successful response.
func TestEgressRotatesOn429(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	var mu sync.Mutex
	var seenAuth []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n := len(seenAuth)
		seenAuth = append(seenAuth, r.Header.Get("Authorization"))
		mu.Unlock()
		if n == 0 {
			// First key is rate-limited.
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":"rate_limited"}`)
			return
		}
		// Second key succeeds.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"ok","choices":[]}`)
	}))
	defer upstream.Close()

	pool := NewKeyPool()
	pool.Set("groq", []string{"key-one", "key-two"})
	e := &EgressHandler{
		Store:  st,
		Config: FallbackConfig{GroqURL: upstream.URL},
		Keys:   pool,
	}

	body := `{"model":"groq/llama-3.1-70b","messages":[{"role":"user","content":"hi"}]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	e.ServeGroq(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("client status = %d, want 200 (rotation should hide the 429)", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"ok"`) {
		t.Fatalf("client body = %q, want the success body", w.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seenAuth) != 2 {
		t.Fatalf("upstream saw %d requests, want 2 (one per key)", len(seenAuth))
	}
	if seenAuth[0] == seenAuth[1] {
		t.Fatalf("both attempts used the same key %q — keys did not rotate", seenAuth[0])
	}
	if seenAuth[0] != "Bearer key-one" || seenAuth[1] != "Bearer key-two" {
		t.Fatalf("rotation order wrong: %v", seenAuth)
	}
}

// TestEgressSurfacesFinal429 confirms that when every key is exhausted the
// real upstream error reaches the client rather than being swallowed.
func TestEgressSurfacesFinal429(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	var hits int
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":"rate_limited"}`)
	}))
	defer upstream.Close()

	pool := NewKeyPool()
	pool.Set("groq", []string{"key-one", "key-two"})
	e := &EgressHandler{Store: st, Config: FallbackConfig{GroqURL: upstream.URL}, Keys: pool}

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"groq/llama"}`))
	w := httptest.NewRecorder()
	e.ServeGroq(w, r)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("client status = %d, want 429 surfaced after all keys exhausted", w.Code)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 2 {
		t.Fatalf("upstream hits = %d, want 2 (tried both keys)", hits)
	}
}
