package llamacpp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// PLAN T29.5: llama-server names its version by build, in /props
// `build_info` — "b11028-972d2313b" on the design-partner cell, the same
// build `llama-server --version` printed.
func TestVersionIsPropsBuildInfo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/props" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"total_slots":18,"build_info":"b11028-972d2313b"}`))
	}))
	defer srv.Close()
	if v, err := New(srv.URL).Version(context.Background()); err != nil || v != "b11028-972d2313b" {
		t.Fatalf("version %q %v", v, err)
	}
}
