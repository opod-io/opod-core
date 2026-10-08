package sglang

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// SGLang's slot count is its scheduler's EFFECTIVE running-request limit,
// summed over data-parallel ranks; an older release's max_running_requests
// stands in, and the deprecated path answers when the new one does not
// (ADR-091).
func TestSlotsFromServerInfo(t *testing.T) {
	cases := []struct {
		path, body string
		want       int
	}{
		{"/server_info", `{"internal_states":[{"effective_max_running_requests_per_dp":48},{"effective_max_running_requests_per_dp":48}]}`, 96},
		{"/get_server_info", `{"internal_states":[{"max_running_requests":32}]}`, 32},
	}
	for _, c := range cases {
		hits := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != c.path {
				http.NotFound(w, r)
				return
			}
			hits++
			_, _ = w.Write([]byte(c.body))
		}))
		d := New(srv.URL, "")
		for i := 0; i < 2; i++ {
			n, ok := d.Slots(context.Background())
			if !ok || n != c.want {
				t.Fatalf("%s: got %d %v, want %d", c.path, n, ok, c.want)
			}
		}
		if hits != 1 {
			t.Fatalf("%s: the slot count is read once and kept, read %d times", c.path, hits)
		}
		srv.Close()
	}
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	if n, ok := New(srv.URL, "").Slots(context.Background()); ok || n != 0 {
		t.Fatalf("a server that does not say: got %d %v", n, ok)
	}
}
