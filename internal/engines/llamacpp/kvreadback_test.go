package llamacpp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// PLAN T23.2: llama-server's /props gives ONE slot's context. The pool is that
// times the slots when each slot has its own partition, and that alone when
// they share a unified pool — which only the launching worker knows. With one
// slot the two agree; with several and no word on the layout, no pool is
// stated rather than one off by the slot count. llama.cpp has no preemption
// and counts neither tokens in use nor evictions.
func TestTheKVPoolFollowsTheSlotLayout(t *testing.T) {
	props := func(slots, nctx int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/metrics":
				_, _ = w.Write([]byte("llamacpp:requests_processing 1\n"))
			case "/props":
				_, _ = w.Write([]byte(`{"default_generation_settings":{"n_ctx":` + strconv.Itoa(nctx) + `},"total_slots":` + strconv.Itoa(slots) + `}`))
			default:
				http.NotFound(w, r)
			}
		}))
	}
	for _, c := range []struct {
		name        string
		slots, nctx int
		unified     *bool
		want        int64 // 0 = not stated
	}{
		{"one slot", 1, 8192, nil, 8192},
		{"partitioned", 4, 8192, new(false), 4 * 8192},
		{"unified", 4, 32768, new(true), 32768},
		{"several slots, layout not told", 4, 8192, nil, 0},
		{"no context stated", 4, 0, new(false), 0},
	} {
		srv := props(c.slots, c.nctx)
		d := New(srv.URL)
		if c.unified != nil {
			d.SetKVUnified(*c.unified)
		}
		ld, err := d.Load(context.Background())
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case c.want == 0 && ld.KVCapacityTokens != nil:
			t.Errorf("%s: pool %d, want none stated", c.name, *ld.KVCapacityTokens)
		case c.want != 0 && (ld.KVCapacityTokens == nil || *ld.KVCapacityTokens != c.want):
			t.Errorf("%s: pool %v, want %d", c.name, ld.KVCapacityTokens, c.want)
		}
		if ld.PreemptionsTotal != nil || ld.KVUsedTokens != nil || ld.EvictionsTotal != nil {
			t.Errorf("%s: llama.cpp says none of these: %+v", c.name, ld)
		}
	}
}
