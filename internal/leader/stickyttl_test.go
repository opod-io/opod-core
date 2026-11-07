package leader

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/opod-io/opod-core/internal/config"
	"github.com/opod-io/opod-core/internal/store"
)

// PLAN T24.55: the policy's routing.stickyTtlSeconds sets the pin lifetime
// live; 0 gives back the leader's start value, so a hand-configured leader
// loses nothing to a policy that says nothing.
func TestThePolicySetsTheStickyTTLLive(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start int // OPOD_STICKY_SESSION_TTL_SECONDS
	}{
		{"started with no TTL", 0},
		{"started with 300 s", 300},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Listen = ":0"
			cfg.Router.StickySessionTTLSeconds = tc.start
			st, err := store.OpenSQLite(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			srv := NewServer(cfg, st, &stubLeaderEngine{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
			start := time.Duration(tc.start) * time.Second
			if got := srv.router.StickyTTL(); got != start {
				t.Fatalf("at start: %s, want %s", got, start)
			}
			srv.applyPolicySnapshot(&PolicySnapshot{Revision: "p1", Routing: PolicyRouting{StickyTTLSeconds: 900}})
			if got := srv.router.StickyTTL(); got != 15*time.Minute {
				t.Fatalf("after a policy of 900 s: %s, want 15m", got)
			}
			srv.applyPolicySnapshot(&PolicySnapshot{Revision: "p2"})
			if got := srv.router.StickyTTL(); got != start {
				t.Fatalf("a policy that stops saying it must give back the start value %s, got %s", start, got)
			}
		})
	}
}
