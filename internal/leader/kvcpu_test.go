package leader

import (
	"io"
	"log/slog"
	"testing"

	"github.com/opod-io/opod-sdk/adminapi"
	"github.com/opod-io/opod-sdk/nodeapi"
	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/store"
)

// PLAN T24.56: the index keeps a worker's CPU-tier blocks beside its card's;
// a chain is held "any" while each leading block is on the card or below it.
func TestThePrefixIndexKeepsTheCPUTier(t *testing.T) {
	p := newPrefixIndex()
	p.apply("w", &nodeapi.KVBlocks{Seq: 1, BlockSize: 16, Stored: []string{"a", "b"}, CPUStored: []string{"a", "b", "c"}})
	p.apply("w", &nodeapi.KVBlocks{Seq: 2, Removed: []string{"a", "b"}}) // the card evicts; the CPU tier keeps them
	chain := []string{"a", "b", "c", "d"}
	if n := p.leading("w", chain); n != 0 {
		t.Fatalf("card leading = %d, want 0 after the eviction", n)
	}
	if n := p.leadingAny("w", chain); n != 3 {
		t.Fatalf("card-or-CPU leading = %d, want 3", n)
	}
	if st := p.state(); st == nil || st.CPUBlocks != 3 || st.Blocks != 0 {
		t.Fatalf("/loadz state %+v, want 3 CPU blocks and 0 on the card", st)
	}
	p.apply("w", &nodeapi.KVBlocks{Seq: 3, CPURemoved: []string{"b"}})
	if n := p.leadingAny("w", chain); n != 1 {
		t.Fatalf("after the CPU tier drops b: %d, want 1 (the chain breaks at b)", n)
	}
	p.apply("w", &nodeapi.KVBlocks{Seq: 4, Cleared: true})
	if st := p.state(); st.CPUBlocks != 0 {
		t.Fatalf("cleared must forget the CPU tier too: %+v", st)
	}
}

// The policy turns the CPU credit on for cpu-lower and off for gpu or unset.
func TestThePolicyChoosesTheBlockTier(t *testing.T) {
	cfg := config.Default()
	cfg.Listen = ":0"
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := NewServer(cfg, st, &stubLeaderEngine{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	for _, tc := range []struct {
		tier string
		want bool
	}{{adminapi.PrefixBlockTierCPULower, true}, {adminapi.PrefixBlockTierGPU, false}, {adminapi.PrefixBlockTierCPULower, true}, {"", false}} {
		srv.applyPolicySnapshot(&PolicySnapshot{Revision: "r-" + tc.tier, Routing: PolicyRouting{PrefixBlockWeight: 0.5, PrefixBlockTier: tc.tier}})
		if got := srv.router.PrefixBlockCPU(); got != tc.want {
			t.Errorf("prefixBlockTier %q: CPU credit %v, want %v", tc.tier, got, tc.want)
		}
	}
}
