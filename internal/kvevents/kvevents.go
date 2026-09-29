// Package kvevents turns an engine's prefix-cache events into the block
// hashes a worker reports on its heartbeat (feature "kv_block_events").
//
// vLLM publishes what its KV cache stores and evicts on a ZeroMQ PUB socket
// that CONNECTS to the worker's SUB socket on localhost
// (`--kv-events-config`), msgpack-encoded: a batch `[ts, [event…], rank?]`
// whose events are tagged arrays — `["BlockStored", block_hashes,
// parent_block_hash, token_ids, block_size, …]`, `["BlockRemoved",
// block_hashes, …]`, `["AllBlocksCleared"]`. Frames are `(topic, seq, payload)`
// with seq an 8-byte big-endian counter.
//
// What leaves this package is OUR hash of each block (nodeapi.BlockHash over
// the parent's hash and the block's token ids), never the engine's hash —
// which is internal to it and has changed between versions — and never a
// token id. The engine's hash is kept only as the key that lets a later
// BlockRemoved, which names blocks by the engine's hash alone, find ours.
//
// Anything that could leave the translation wrong — a gap in the engine's
// sequence, a block whose parent we never saw, a payload we cannot read —
// resolves one way: Cleared, and start over. A leader with an empty index
// routes by the sticky pin; one with a wrong index routes worse than that.
package kvevents

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-zeromq/zmq4"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/opod-io/opod-sdk/nodeapi"
)

const (
	// DefaultEndpoint is where the worker tells its engine to publish, and
	// where it listens: localhost, like every engine port.
	DefaultEndpoint = "tcp://127.0.0.1:5557"
	// Topic is the subscription prefix the engine is configured with.
	Topic = "kv-events"
	// maxKnown bounds the engine-hash → our-hash table.
	maxKnown = 400_000
	// maxPending bounds what one heartbeat carries; past it the batch is a
	// Cleared and the index is rebuilt from what follows.
	maxPending = 50_000
)

// Translator turns decoded engine batches into pending block changes. It is
// the whole logic and has no socket in it.
type Translator struct {
	mu        sync.Mutex
	known     map[string]string // engine hash → our hash
	order     []string          // engine hashes, oldest first, for the bound
	stored    []string
	removed   []string
	cleared   bool
	blockSize int
	seq       int64 // batches handed to the heartbeat
	lastEng   int64 // the engine's last frame sequence, -1 before the first
	events    int64
}

// NewTranslator returns an empty translator.
func NewTranslator() *Translator {
	return &Translator{known: map[string]string{}, lastEng: -1}
}

// reset forgets everything and owes the leader a Cleared. Caller holds mu.
func (t *Translator) reset() {
	t.known, t.order = map[string]string{}, nil
	t.stored, t.removed, t.cleared = nil, nil, true
}

// Frame applies one published frame: the engine's sequence and its payload.
func (t *Translator) Frame(engineSeq int64, payload []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.lastEng >= 0 && engineSeq != t.lastEng+1 {
		t.reset() // events were lost between the engine and us
	}
	t.lastEng = engineSeq
	// Decoded generically: the batch is positional, its events are tagged
	// arrays whose tail grows with the engine's version, and a hash is an
	// integer or bytes depending on it.
	var v any
	if err := msgpack.Unmarshal(payload, &v); err != nil {
		t.reset()
		return fmt.Errorf("kv event batch: unreadable: %w", err)
	}
	batch, ok := v.([]any)
	if !ok || len(batch) < 2 {
		t.reset()
		return fmt.Errorf("kv event batch: %T of %d fields", v, len(batch))
	}
	events, ok := batch[1].([]any)
	if !ok {
		t.reset()
		return fmt.Errorf("kv event batch: events are %T", batch[1])
	}
	for _, ev := range events {
		if err := t.event(ev); err != nil {
			t.reset()
			return err
		}
	}
	if len(t.stored)+len(t.removed) > maxPending {
		t.reset()
	}
	return nil
}

func (t *Translator) event(ev any) error {
	f, ok := ev.([]any)
	if !ok || len(f) == 0 {
		return fmt.Errorf("kv event: %T is not a tagged array", ev)
	}
	tag, ok := f[0].(string)
	if !ok {
		return fmt.Errorf("kv event: tag is %T", f[0])
	}
	t.events++
	switch tag {
	case "BlockStored":
		// [tag, block_hashes, parent_block_hash, token_ids, block_size, …]
		if len(f) < 5 {
			return fmt.Errorf("BlockStored: %d fields", len(f))
		}
		hashes, err := hashList(f[1])
		if err != nil {
			return fmt.Errorf("BlockStored: block_hashes: %w", err)
		}
		parent, err := hashKey(f[2])
		if err != nil {
			return fmt.Errorf("BlockStored: parent_block_hash: %w", err)
		}
		tokens, err := intList(f[3])
		if err != nil {
			return fmt.Errorf("BlockStored: token_ids: %w", err)
		}
		size64, ok := asInt(f[4])
		size := int(size64)
		if !ok || size <= 0 {
			return fmt.Errorf("BlockStored: block_size %v", f[4])
		}
		t.blockSize = size
		ours := ""
		if parent != "" {
			p, ok := t.known[parent]
			if !ok {
				return nil // a block whose prefix we never saw: not ours to name, and harmless to skip
			}
			ours = p
		}
		for i, h := range hashes {
			lo, hi := i*size, (i+1)*size
			if hi > len(tokens) {
				break
			}
			ours = nodeapi.BlockHash(ours, tokens[lo:hi])
			if _, seen := t.known[h]; !seen {
				t.order = append(t.order, h)
			}
			t.known[h] = ours
			t.stored = append(t.stored, ours)
		}
		for len(t.order) > maxKnown {
			delete(t.known, t.order[0])
			t.order = t.order[1:]
		}
	case "BlockRemoved":
		if len(f) < 2 {
			return fmt.Errorf("BlockRemoved: %d fields", len(f))
		}
		hashes, err := hashList(f[1])
		if err != nil {
			return fmt.Errorf("BlockRemoved: block_hashes: %w", err)
		}
		for _, h := range hashes {
			if ours, ok := t.known[h]; ok {
				t.removed = append(t.removed, ours)
				delete(t.known, h)
			}
		}
	case "AllBlocksCleared":
		t.reset()
	default:
		// A newer engine's event we do not know: no statement, not an error.
	}
	return nil
}

// asInt reads any msgpack integer.
func asInt(v any) (int64, bool) {
	switch x := v.(type) {
	case int8:
		return int64(x), true
	case int16:
		return int64(x), true
	case int32:
		return int64(x), true
	case int64:
		return x, true
	case int:
		return int64(x), true
	case uint8:
		return int64(x), true
	case uint16:
		return int64(x), true
	case uint32:
		return int64(x), true
	case uint64:
		return int64(x), true
	case uint:
		return int64(x), true
	}
	return 0, false
}

func intList(v any) ([]int, error) {
	items, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%T is not a list", v)
	}
	out := make([]int, len(items))
	for i, it := range items {
		n, ok := asInt(it)
		if !ok {
			return nil, fmt.Errorf("element %d is %T", i, it)
		}
		out[i] = int(n)
	}
	return out, nil
}

// hashKey reads one engine block hash — an integer or bytes, by version — as
// a map key. nil reads as "" (no parent).
func hashKey(v any) (string, error) {
	switch x := v.(type) {
	case nil:
		return "", nil
	case []byte:
		return "b:" + string(x), nil
	case string:
		return "s:" + x, nil
	}
	if n, ok := asInt(v); ok {
		return fmt.Sprintf("i:%d", n), nil
	}
	return "", fmt.Errorf("a block hash of type %T", v)
}

func hashList(v any) ([]string, error) {
	items, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%T is not a list", v)
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		k, err := hashKey(it)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, nil
}

// Drain hands over what changed since the last call, or nil when nothing did.
func (t *Translator) Drain() *nodeapi.KVBlocks {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.cleared && len(t.stored) == 0 && len(t.removed) == 0 {
		return nil
	}
	t.seq++
	out := &nodeapi.KVBlocks{Seq: t.seq, BlockSize: t.blockSize, Stored: t.stored, Removed: t.removed, Cleared: t.cleared}
	t.stored, t.removed, t.cleared = nil, nil, false
	return out
}

// Events is how many engine events were read (a gauge for the worker's log).
func (t *Translator) Events() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.events
}

// Subscribe receives the engine's events until ctx ends, feeding t. It binds
// the endpoint and the engine's publisher connects to it; a socket that had to
// be reopened has lost events, so every reopen is a reset. An engine that is
// restarted reconnects to the same socket on its own, and its sequence starts
// over — which Frame reads as a gap and answers the same way.
func Subscribe(ctx context.Context, endpoint string, t *Translator, logf func(string, ...any)) {
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	// The engine starts minutes after the worker and is restarted under it, so
	// "nothing listens yet" is the normal state for a while: said once, then
	// once a minute, never every three seconds.
	var lastSaid time.Time
	for ctx.Err() == nil {
		if err := listen(ctx, endpoint, t, logf); err != nil && ctx.Err() == nil && time.Since(lastSaid) > time.Minute {
			logf("kv events: %v — reopening in 3 s", err)
			lastSaid = time.Now()
		}
		t.mu.Lock()
		t.reset()
		t.lastEng = -1
		t.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
		}
	}
}

func listen(ctx context.Context, endpoint string, t *Translator, logf func(string, ...any)) error {
	var lastSaid time.Time
	sub := zmq4.NewSub(ctx)
	defer func() { _ = sub.Close() }()
	// The worker LISTENS and the engine connects. vLLM's publisher binds only
	// when its endpoint carries a wildcard ("tcp://*:5557") and otherwise
	// connects out — and a wildcard bind would put a stream that carries token
	// ids on the pod's network. A plain localhost endpoint keeps it inside the
	// pod, which means this side is the one that binds. (Found on a cluster:
	// dialling got "connection refused" for as long as the engine ran.)
	if err := sub.Listen(endpoint); err != nil {
		return err
	}
	if err := sub.SetOption(zmq4.OptionSubscribe, Topic); err != nil {
		return err
	}
	for {
		msg, err := sub.Recv()
		if err != nil {
			return err
		}
		// (topic, seq, payload)
		if len(msg.Frames) < 3 || !bytes.HasPrefix(msg.Frames[0], []byte(Topic)) || len(msg.Frames[1]) != 8 {
			continue
		}
		seq := int64(binary.BigEndian.Uint64(msg.Frames[1]))
		// An unreadable batch already reset the translator; what is left is to
		// SAY so, or a worker reports nothing for ever and nobody knows why.
		// The error names shapes and types, never a value from the payload.
		if err := t.Frame(seq, msg.Frames[2]); err != nil && time.Since(lastSaid) > time.Minute {
			logf("kv events: a batch could not be read, so this worker reports no blocks: %v (layout: %s)", err, Layout(msg.Frames[2]))
			lastSaid = time.Now()
		}
	}
}

// Layout describes a payload's structure — the type of each field, the length
// of each list, the tag of each event — and none of its values: enough to see
// that an engine's version changed the wire, with nothing of a prompt in it.
func Layout(payload []byte) string {
	var v any
	if err := msgpack.Unmarshal(payload, &v); err != nil {
		return "not msgpack: " + err.Error()
	}
	return shape(v, 0)
}

func shape(v any, depth int) string {
	switch x := v.(type) {
	case []any:
		if depth >= 3 || len(x) == 0 {
			return fmt.Sprintf("list[%d]", len(x))
		}
		// A long homogeneous list (token ids, hashes) is described by its first element.
		if len(x) > 8 {
			return fmt.Sprintf("list[%d of %s]", len(x), shape(x[0], depth+1))
		}
		parts := make([]string, len(x))
		for i, e := range x {
			if s, ok := e.(string); ok && i == 0 && depth > 0 {
				parts[i] = "tag " + s // an event's tag is a type name, not content
				continue
			}
			parts[i] = shape(e, depth+1)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k, e := range x {
			keys = append(keys, k+":"+shape(e, depth+1))
		}
		sort.Strings(keys)
		return "map{" + strings.Join(keys, ", ") + "}"
	case nil:
		return "nil"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// EngineConfig is the JSON the engine is started with (`--kv-events-config`).
func EngineConfig(endpoint string) string {
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	// A plain localhost endpoint: the engine CONNECTS to it (it would bind only
	// for a wildcard), so the stream never leaves the pod.
	return fmt.Sprintf(`{"enable_kv_cache_events":true,"publisher":"zmq","endpoint":%q,"topic":%q}`, endpoint, Topic)
}
