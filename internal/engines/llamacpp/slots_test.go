package llamacpp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// llama.cpp's slot count is /props total_slots (ADR-091).
func TestSlotsFromProps(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/props" {
			_, _ = w.Write([]byte(`{"total_slots":4}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	if n, ok := New(srv.URL).Slots(context.Background()); !ok || n != 4 {
		t.Fatalf("want 4 slots, got %d %v", n, ok)
	}
}
