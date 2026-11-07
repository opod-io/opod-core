package nodeapi

import "testing"

// The block hash is a wire contract: a worker and a leader built from
// different commits must agree on it for ever, so its values are pinned here.
func TestBlockHashIsStableAndChained(t *testing.T) {
	toks := make([]int, 40)
	for i := range toks {
		toks[i] = i * 7
	}
	got := BlockHashes(toks, 16)
	if len(got) != 2 {
		t.Fatalf("40 tokens at block size 16 are two full blocks, got %d", len(got))
	}
	want := []string{BlockHash("", toks[:16]), BlockHash(BlockHash("", toks[:16]), toks[16:32])}
	if got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("chain %v, want %v", got, want)
	}
	// Pinned values: changing the function changes these, and that is a
	// protocol change, not a refactor.
	if got[0] != pinnedFirst || got[1] != pinnedSecond {
		t.Fatalf("the hash moved: %q %q", got[0], got[1])
	}
	// The same block after a different prefix is a different hash: a hash
	// names a whole prefix.
	other := append([]int{999}, toks[1:]...)
	if BlockHashes(other, 16)[1] == got[1] {
		t.Fatal("a block's hash must depend on everything before it")
	}
	if len(BlockHashes(toks, 0)) != 0 || len(BlockHashes(toks[:15], 16)) != 0 {
		t.Fatal("no block size, or less than one block, is no blocks")
	}
}
