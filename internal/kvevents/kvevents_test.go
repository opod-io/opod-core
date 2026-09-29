package kvevents

import (
	"context"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/go-zeromq/zmq4"

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

// The wire, end to end: the worker binds, a publisher CONNECTS to it as vLLM
// does for a plain endpoint, and a frame it sends arrives as block hashes.
func TestSubscribeBindsAndAPublisherConnects(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "tcp://" + ln.Addr().String()
	ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tr := NewTranslator()
	go Subscribe(ctx, endpoint, tr, func(string, ...any) {})

	pub := zmq4.NewPub(ctx)
	defer pub.Close()
	deadline := time.Now().Add(5 * time.Second)
	for pub.Dial(endpoint) != nil {
		if time.Now().After(deadline) {
			t.Fatal("the subscriber never bound its endpoint")
		}
		time.Sleep(50 * time.Millisecond)
	}
	payload := batch(t, []any{"BlockStored", []any{int64(1)}, nil, toks(0, 16), 16})
	for seq := int64(0); time.Now().Before(deadline); seq++ {
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(seq))
		_ = pub.Send(zmq4.NewMsgFrom([]byte(Topic), b[:], payload))
		if got := tr.Drain(); got != nil && len(got.Stored) > 0 {
			if got.Stored[0] != nodeapi.BlockHash("", toks(0, 16)) {
				t.Fatalf("stored %v", got.Stored)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("no block arrived through the socket")
}

// The layout vLLM 0.27.1 actually sends, measured on a cluster: each event a
// MAP with its tag under "type", hashes as uint64, fields this package never
// reads beside the ones it does.
func TestTranslatorReadsTheMapLayout(t *testing.T) {
	tr := NewTranslator()
	stored := func(hashes []any, parent any, tokens []int) map[string]any {
		return map[string]any{"type": "BlockStored", "block_hashes": hashes, "parent_block_hash": parent,
			"token_ids": tokens, "block_size": int8(16), "lora_id": nil, "lora_name": nil, "medium": "gpu",
			"extra_keys": make([]any, len(hashes)), "group_idx": int8(0), "kv_cache_spec_kind": "full"}
	}
	big := uint64(1)<<63 | 12 // a hash with the top bit set
	first := toks(100, 32)
	b, err := msgpack.Marshal([]any{1790000000.5, []any{stored([]any{uint64(11), big}, nil, first)}, int8(0)})
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Frame(0, b); err != nil {
		t.Fatal(err)
	}
	next := toks(200, 16)
	b, _ = msgpack.Marshal([]any{1790000001.5, []any{stored([]any{uint64(13)}, big, next)}, int8(0)})
	if err := tr.Frame(1, b); err != nil {
		t.Fatal(err)
	}
	got := tr.Drain()
	want := nodeapi.BlockHashes(append(append([]int{}, first...), next...), 16)
	if got == nil || got.Cleared || got.BlockSize != 16 || len(got.Stored) != 3 {
		t.Fatalf("drain %+v", got)
	}
	for i := range want {
		if got.Stored[i] != want[i] {
			t.Fatalf("block %d: %s, want %s", i, got.Stored[i], want[i])
		}
	}
	b, _ = msgpack.Marshal([]any{1790000002.5, []any{map[string]any{"type": "BlockRemoved", "block_hashes": []any{big}, "medium": "gpu"}}, int8(0)})
	if err := tr.Frame(2, b); err != nil {
		t.Fatal(err)
	}
	if got = tr.Drain(); got == nil || len(got.Removed) != 1 || got.Removed[0] != want[1] {
		t.Fatalf("removal %+v, want [%s]", got, want[1])
	}
}
