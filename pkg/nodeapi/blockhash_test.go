package nodeapi

import "testing"

// The block hash is a wire contract: a worker and a leader built from
// different commits must agree on it for ever, so its values are pinned here.
func TestBlockHashIsStableAndChained(t *testing.T) {
	toks := make([]int, 40)
	for i := range toks {
		toks[i] = i * 7
	}
	got := BlockHashes("", toks, 16)
	if len(got) != 2 {
		t.Fatalf("40 tokens at block size 16 are two full blocks, got %d", len(got))
	}
	want := []string{BlockHash("", "", toks[:16]), BlockHash(BlockHash("", "", toks[:16]), "", toks[16:32])}
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
	if BlockHashes("", other, 16)[1] == got[1] {
		t.Fatal("a block's hash must depend on everything before it")
	}
	if len(BlockHashes("", toks, 0)) != 0 || len(BlockHashes("", toks[:15], 16)) != 0 {
		t.Fatal("no block size, or less than one block, is no blocks")
	}
}

// A seeded chain (T28.9): the first block's keys — a cache salt, a LoRA name —
// make the same tokens a different chain, the seed rides every later block
// through its parent, and no seed is exactly the pinned chain above.
func TestBlockHashSeed(t *testing.T) {
	toks := make([]int, 40)
	for i := range toks {
		toks[i] = i * 7
	}
	plain := BlockHashes("", toks, 16)
	if BlockSeed(nil) != "" || BlockSeed([]any{}) != "" {
		t.Fatal("no keys is no seed")
	}
	if got := BlockHashes(BlockSeed(nil), toks, 16); got[0] != pinnedFirst || got[1] != pinnedSecond {
		t.Fatalf("an empty seed moved the pinned chain: %v", got)
	}
	a, a2, b := BlockHashes(BlockSeed([]any{"salt-a"}), toks, 16), BlockHashes(BlockSeed([]any{"salt-a"}), toks, 16), BlockHashes(BlockSeed([]any{"salt-b"}), toks, 16)
	for i := range plain {
		if a[i] != a2[i] {
			t.Fatalf("block %d: one seed, two chains", i)
		}
		if a[i] == b[i] || a[i] == plain[i] {
			t.Fatalf("block %d is shared across seeds", i)
		}
	}
	// The seed is read for the first block only: a later block continues
	// whatever chain its parent names.
	if BlockHash(a[0], "ignored", toks[16:32]) != a[1] {
		t.Fatal("a later block's hash depends on its parent, not on a seed")
	}
	// One canonical encoding: the widths a msgpack decoder picks for one
	// integer encode alike; order, kind and nesting do not.
	if BlockSeed([]any{"x", int8(5)}) != BlockSeed([]any{"x", uint64(5)}) {
		t.Fatal("one integer, two encodings")
	}
	distinct := [][]any{
		{"lora", "salt"}, {"salt", "lora"}, {"lorasalt"}, {"lora"}, {[]byte("lora"), "salt"},
		{[]any{"lora", "salt"}}, {"x", int64(5)}, {"x", "5"}, {nil}, {map[string]any{"k": "v"}},
	}
	seen := map[string]int{}
	for i, k := range distinct {
		s := BlockSeed(k)
		if j, dup := seen[s]; dup {
			t.Fatalf("keys %v and %v share an encoding", distinct[j], k)
		}
		seen[s] = i
	}
	if BlockSeed([]any{map[string]any{"a": 1, "b": 2}}) != BlockSeed([]any{map[string]any{"b": 2, "a": 1}}) {
		t.Fatal("a map's encoding must not depend on its iteration order")
	}
}
