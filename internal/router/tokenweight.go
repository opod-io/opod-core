package router

// Token-weighted in-flight load (PLAN T28.10, ADR-122 §2; feature
// "routing_token_weight").
//
// The rank counts each request in flight as 1, so a 100 000-token prefill and
// a 50-token chat weigh the same, and a burst of long prompts can land on one
// worker: new requests queue behind the prefills, and the prefills slow every
// decode step there. With routing.tokenWeight set, each request also carries
// its estimated UNCACHED prompt tokens from the moment it is dispatched until
// its first streamed event (a non-streamed answer: the answer itself), or
// until it fails or is cancelled — the prefill window, as llm-d's
// inflight-load-producer counts it. After the first event the cost is decode,
// which the request count and the KV term already see.
//
//   score += tokenWeight × (uncached prompt tokens in prefill) / 1000
//
// so tokenWeight is what 1,000 uncached prompt tokens weigh against one
// request in flight. 0 (the default) holds nothing and the rank is exactly
// the request count.
//
// The estimate: prompt bytes ÷ 4 (no tokenizer on the request path), or the
// prompt's full blocks in the block index's chain when that is more — the
// chain is the engine's own tokens, but it stops at the index's tokenize cap,
// so for a long prompt it is a floor and bytes ÷ 4 is the figure. The leading
// blocks the target worker already holds are taken off: its engine does not
// compute them again.

import (
	"context"
	"strings"
	"sync"

	"github.com/opod-io/opod-core/internal/engines"
)

const (
	// tokenWeightUnit: routing.tokenWeight is per this many tokens.
	tokenWeightUnit = 1000
	// bytesPerToken is the estimate with no tokenizer: English text and code
	// run close to four bytes a token on the tokenizers the catalog serves.
	bytesPerToken = 4
)

// SetTokenWeight applies routing.tokenWeight. weight ≤ 0 turns the ledger
// off: nothing is held, and holds already taken release as usual. blockSize
// answers the block size a worker reports its cache blocks in (0 = unknown,
// and then no held block is taken off the estimate); nil = unknown for all.
func (r *Router) SetTokenWeight(weight float64, blockSize func(nodeID string) int) {
	if weight < 0 {
		weight = 0
	}
	r.mu.Lock()
	r.tokenWeight, r.blockSizeOf = weight, blockSize
	r.mu.Unlock()
}

// TokenWeight reports the applied weight (for /admin/v1 and tests).
func (r *Router) TokenWeight() float64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.tokenWeight
}

// prefillTokens returns a snapshot of the uncached prompt tokens held per
// router key (node id or gang key) — requests dispatched whose first event
// has not come back.
func (r *Router) prefillTokens() map[string]int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]int, len(r.prefill))
	for k, v := range r.prefill {
		out[k] = v
	}
	return out
}

// promptBytes is the size of what an engine renders into the prompt: the
// system prompt, every turn's text and tool calls, and the tool list. Images
// are left out — their token cost depends on the model, not their bytes.
func promptBytes(req engines.ChatRequest) int {
	n := len(req.System) + len(req.Tools)
	for _, m := range req.Messages {
		n += len(m.Content) + len(m.ToolCalls)
	}
	return n
}

// uncachedTokens estimates the prompt tokens key's engine must compute for
// req (see the file comment). blocksNode is the node whose block index
// stands for key: the worker itself, or a gang's head; "" = none.
func (r *Router) uncachedTokens(ctx context.Context, req engines.ChatRequest, blocksNode string) int {
	tokens := promptBytes(req) / bytesPerToken
	hashes := prefixBlocksFrom(ctx)
	if len(hashes) == 0 || blocksNode == "" {
		return tokens
	}
	r.mu.RLock()
	sizeOf, held, heldCPU := r.blockSizeOf, r.blockHeld, r.blockHeldCPU
	r.mu.RUnlock()
	if sizeOf == nil {
		return tokens
	}
	bs := sizeOf(blocksNode)
	if bs <= 0 {
		return tokens
	}
	tokens = max(tokens, len(hashes)*bs)
	have := 0
	if held != nil {
		have = held(blocksNode, hashes)
	}
	if heldCPU != nil { // cpu-lower: a host-memory block is copied back, not computed
		have = max(have, heldCPU(blocksNode, hashes))
	}
	return max(0, tokens-have*bs)
}

// blocksNodeOf is the node whose block index stands for a router key: a
// worker's own id, or for a gang the head the pick chose (trace.go).
func blocksNodeOf(ctx context.Context, key string) string {
	if isGangKey(key) {
		return gangHeadFrom(ctx)
	}
	return key
}

// isGangKey reports whether a router key names a gang (gangKey), not a worker.
func isGangKey(key string) bool { return strings.HasPrefix(key, "shard:") }

// prefillHold is one request's tokens in the ledger. release is safe to call
// any number of times from any path: only the first takes the tokens off.
type prefillHold struct {
	once    sync.Once
	release func()
}

// Release takes the hold's tokens off the ledger; later calls do nothing. A
// nil hold is a no-op, so every exit path may call it.
func (h *prefillHold) Release() {
	if h != nil {
		h.once.Do(h.release)
	}
}

// holdPrefill puts tokens on key's ledger until the returned hold is
// released. With the weight off, or nothing to hold, it holds nothing (a nil
// hold) — the ledger is then exactly today's: empty.
func (r *Router) holdPrefill(key string, tokens int) *prefillHold {
	if tokens <= 0 {
		return nil
	}
	r.mu.Lock()
	if r.tokenWeight <= 0 {
		r.mu.Unlock()
		return nil
	}
	r.prefill[key] += tokens
	r.mu.Unlock()
	return &prefillHold{release: func() {
		r.mu.Lock()
		if r.prefill[key] -= tokens; r.prefill[key] <= 0 {
			delete(r.prefill, key) // a key with nothing held leaves the map
		}
		r.mu.Unlock()
	}}
}

// dispatchTokens is the uncached prompt tokens one dispatch of req to key
// makes its engine compute: 0 for a decode half handed its KV by a prefill
// worker (pdhandoff.go), -1 when nothing reads the estimate
// (routing.tokenWeight and routing.slowFactor both 0), which skips it.
func (r *Router) dispatchTokens(ctx context.Context, key string, req engines.ChatRequest) int {
	r.mu.RLock()
	on := r.tokenWeight > 0 || r.slow.factor > 0
	r.mu.RUnlock()
	switch {
	case !on:
		return -1
	case engines.HasJSON(req.KVTransfer):
		return 0
	}
	return r.uncachedTokens(ctx, req, blocksNodeOf(ctx, key))
}

// prefillScoreLocked is key's token term of the rank. Caller holds r.mu.
func (r *Router) prefillScoreLocked(key string) float64 {
	if r.tokenWeight <= 0 {
		return 0
	}
	return r.tokenWeight * float64(r.prefill[key]) / tokenWeightUnit
}
