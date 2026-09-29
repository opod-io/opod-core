package kvevents

import (
	"testing"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/opod-io/opod-sdk/nodeapi"
)

func batch(t *testing.T, events ...any) []byte {
	t.Helper()
	b, err := msgpack.Marshal([]any{1790000000.5, events})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func toks(from, n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = from + i
	}
	return out
}

// Engine events become OUR chained hashes: a block stored after a parent we
// know continues that parent's chain, a removal is translated through the
// engine's hash, and neither a token id nor the engine's hash leaves.
func TestTranslatorChainsAndTranslates(t *testing.T) {
	tr := NewTranslator()
	first := toks(100, 32) // two blocks of 16
	if err := tr.Frame(0, batch(t, []any{"BlockStored", []any{int64(11), int64(12)}, nil, first, 16, nil, "gpu"})); err != nil {
		t.Fatal(err)
	}
	next := toks(200, 16)
	if err := tr.Frame(1, batch(t, []any{"BlockStored", []any{int64(13)}, int64(12), next, 16})); err != nil {
		t.Fatal(err)
	}
	got := tr.Drain()
	want := nodeapi.BlockHashes(append(append([]int{}, first...), next...), 16)
	if got == nil || got.Seq != 1 || got.BlockSize != 16 || got.Cleared || len(got.Stored) != 3 {
		t.Fatalf("drain %+v", got)
	}
	for i := range want {
		if got.Stored[i] != want[i] {
			t.Fatalf("block %d: %s, want the chain's %s", i, got.Stored[i], want[i])
		}
	}
	if tr.Drain() != nil {
		t.Fatal("nothing changed, nothing to send")
	}
	// Eviction names blocks by the ENGINE's hash; ours goes out.
	if err := tr.Frame(2, batch(t, []any{"BlockRemoved", []any{int64(12), int64(999)}})); err != nil {
		t.Fatal(err)
	}
	got = tr.Drain()
	if got == nil || got.Seq != 2 || len(got.Removed) != 1 || got.Removed[0] != want[1] {
		t.Fatalf("removal %+v, want [%s]", got, want[1])
	}
}

// Whatever could leave the translation wrong ends in Cleared.
func TestTranslatorClearsRatherThanGuess(t *testing.T) {
	stored := func(h int64, parent any, from int) []any {
		return []any{"BlockStored", []any{h}, parent, toks(from, 16), 16}
	}
	t.Run("a gap in the engine's sequence", func(t *testing.T) {
		tr := NewTranslator()
		_ = tr.Frame(0, batch(t, stored(1, nil, 0)))
		tr.Drain()
		_ = tr.Frame(2, batch(t, stored(2, int64(1), 16))) // frame 1 never arrived
		got := tr.Drain()
		if got == nil || !got.Cleared || len(got.Stored) != 0 {
			t.Fatalf("after a gap: %+v — the parent is no longer known, so nothing can be named", got)
		}
	})
	t.Run("the engine cleared its cache", func(t *testing.T) {
		tr := NewTranslator()
		_ = tr.Frame(0, batch(t, stored(1, nil, 0), []any{"AllBlocksCleared"}))
		if got := tr.Drain(); got == nil || !got.Cleared || len(got.Stored) != 0 {
			t.Fatalf("cleared: %+v", got)
		}
	})
	t.Run("an unreadable payload", func(t *testing.T) {
		tr := NewTranslator()
		if err := tr.Frame(0, []byte{0xc1}); err == nil {
			t.Fatal("garbage must be an error")
		}
		if got := tr.Drain(); got == nil || !got.Cleared {
			t.Fatalf("unreadable: %+v", got)
		}
	})
	t.Run("a parent we never saw is skipped, not invented", func(t *testing.T) {
		tr := NewTranslator()
		_ = tr.Frame(0, batch(t, stored(5, int64(4), 0)))
		if got := tr.Drain(); got != nil {
			t.Fatalf("an orphan block must not be reported: %+v", got)
		}
	})
	t.Run("byte hashes and a newer engine's unknown event", func(t *testing.T) {
		tr := NewTranslator()
		if err := tr.Frame(0, batch(t, []any{"BlockStored", []any{[]byte{1, 2}}, nil, toks(0, 16), 16}, []any{"SomethingNew", 1})); err != nil {
			t.Fatal(err)
		}
		if got := tr.Drain(); got == nil || len(got.Stored) != 1 || got.Cleared {
			t.Fatalf("bytes hash: %+v", got)
		}
	})
}
