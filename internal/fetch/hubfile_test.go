package fetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// One bounded GET, the token sent, a refusal named like every other Hub call.
func TestHubSmallFile(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/org/m/resolve/main/config.json":
			_, _ = w.Write([]byte(`{"tie_word_embeddings":true}`))
		case "/org/gated/resolve/main/config.json":
			w.WriteHeader(http.StatusUnauthorized)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	ctx := context.Background()

	b, err := HubSmallFile(ctx, "org/m", "config.json", "", Options{Endpoint: srv.URL, Token: "test-token"})
	if err != nil || string(b) != `{"tie_word_embeddings":true}` {
		t.Fatalf("got %q, %v", b, err)
	}
	if auth != "Bearer test-token" {
		t.Errorf("the token must be sent: %q", auth)
	}
	if _, err := HubSmallFile(ctx, "org/gated", "config.json", "", Options{Endpoint: srv.URL}); RefusalReason(err) != HubTokenMissing {
		t.Errorf("a gated repo without a token is %s, got %v", HubTokenMissing, err)
	}
	if _, err := HubSmallFile(ctx, "org/m", "../config.json", "", Options{Endpoint: srv.URL}); err == nil {
		t.Error("a path-y file name must be refused")
	}
}
