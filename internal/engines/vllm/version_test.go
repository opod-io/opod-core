package vllm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// PLAN T29.5: the version is vLLM's own GET /version, as vLLM 0.33.0 answered
// it on the design-partner cell.
func TestVersionIsWhatTheEngineSays(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/version" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"version":"0.33.0"}`))
	}))
	defer srv.Close()
	if v, err := New(srv.URL, "").Version(context.Background()); err != nil || v != "0.33.0" {
		t.Fatalf("version %q %v", v, err)
	}
}
