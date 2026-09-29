package fetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// gatedHub answers the way the Hub does for a gated repository: 401 to a
// caller with no credential, 401 to a token it does not know, 403 to a good
// token whose account never accepted the terms, and the file to the one that
// did. sawAuth records every Authorization header it was sent.
func gatedHub(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var sawAuth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		sawAuth = append(sawAuth, auth)
		switch auth {
		case "Bearer hf_granted":
			if strings.Contains(r.URL.Path, "/api/models/") {
				_, _ = w.Write([]byte(`{"sha":"abc","siblings":[]}`))
				return
			}
			w.Header().Set("Content-Length", "4")
			if r.Method != http.MethodHead {
				_, _ = w.Write([]byte("GGUF"))
			}
		case "Bearer hf_notgranted":
			w.Header().Set("X-Error-Code", "GatedRepo")
			w.WriteHeader(http.StatusForbidden)
		case "":
			w.Header().Set("X-Error-Code", "GatedRepo")
			w.WriteHeader(http.StatusUnauthorized)
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &sawAuth
}

// A gated repository with no token fails with a NAME, in both fetch modes, and
// the sentence says what to set — not the bare `401 Unauthorized` it used to be.
func TestAGatedRepoWithNoTokenFailsByName(t *testing.T) {
	hub, _ := gatedHub(t)
	dir := t.TempDir()

	_, err := GGUF(context.Background(), "org/gated", "m.gguf", dir, Options{Endpoint: hub.URL})
	if got := RefusalReason(err); got != HubTokenMissing {
		t.Fatalf("one file, no token: reason %q, err %v", got, err)
	}
	for _, want := range []string{HubTokenMissing, "org/gated", "HF_TOKEN", "GatedRepo"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the error must carry %q: %v", want, err)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("a refused fetch must leave nothing behind — no partial, no lock: %v", entries)
	}

	_, err = Snapshot(context.Background(), "org/gated", dir, Options{Endpoint: hub.URL})
	if got := RefusalReason(err); got != HubTokenMissing {
		t.Fatalf("snapshot, no token: reason %q, err %v", got, err)
	}
}

// The three refusals have three remedies, so they must not share a name: a
// missing token is set, a refused one is replaced, and a good one that was
// never granted the repository is fixed on the Hub, not here.
func TestEachHubRefusalHasItsOwnName(t *testing.T) {
	hub, _ := gatedHub(t)
	for token, want := range map[string]string{
		"":              HubTokenMissing,
		"hf_unknown":    HubTokenRefused,
		"hf_notgranted": HubAccessDenied,
	} {
		_, err := GGUF(context.Background(), "org/gated", "m.gguf", t.TempDir(), Options{Endpoint: hub.URL, Token: token})
		if got := RefusalReason(err); got != want {
			t.Errorf("token %q: reason %q, want %q (%v)", token, got, want, err)
		}
		if token != "" && err != nil && strings.Contains(err.Error(), token) {
			t.Errorf("the error must never carry the token it was refused with: %v", err)
		}
	}
	// And a token the repository accepts is no refusal at all.
	if _, err := GGUF(context.Background(), "org/gated", "m.gguf", t.TempDir(), Options{Endpoint: hub.URL, Token: "hf_granted"}); err != nil {
		t.Fatalf("a granted token fetches: %v", err)
	}
}

// A 404 is a wrong name and a 5xx is the Hub's own trouble: neither is about
// the token, and naming them as if they were would send an operator to rotate
// a credential that is fine.
func TestOnlyACredentialRefusalIsNamed(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusInternalServerError, http.StatusTooManyRequests} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }))
		_, err := GGUF(context.Background(), "org/m", "m.gguf", t.TempDir(), Options{Endpoint: srv.URL, Token: "hf_any"})
		srv.Close()
		if err == nil {
			t.Fatalf("%d must fail the fetch", code)
		}
		if got := RefusalReason(err); got != "" {
			t.Fatalf("%d is not a credential refusal, got reason %q: %v", code, got, err)
		}
	}
}
