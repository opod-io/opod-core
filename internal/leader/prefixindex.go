package leader

// The prefix-cache block index (feature "kv_block_events").
//
// Workers whose engine publishes cache events report, on their heartbeat, the
// block hashes their engine stored and evicted (nodeapi.KVBlocks). The leader
// keeps, per worker, the set it holds, and scores a request by how many of its
// LEADING blocks each candidate holds (router/prefixblocks.go).
//
// What the index never holds: token ids or text. A hash names a prefix and
// says nothing about it (ADR-072: request content is not ours to keep).
//
// Bounds. Each worker's set is capped at prefixIndexPerWorker hashes, oldest
// first out: an engine evicts too, and tells us, so the cap only ever bites on
// a worker that stopped saying what it removed. A gap in a worker's sequence,
// a new boot id, or the worker's own `cleared` empties that worker's set — an
// index that is briefly empty routes by the sticky pin, one that is wrong
// routes worse than none.

import (
	"bytes"
	"container/list"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/maphash"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/opod-io/opod-core/internal/auth"
	"github.com/opod-io/opod-core/internal/engines"
	"github.com/opod-io/opod-core/internal/router"
	"github.com/opod-io/opod-core/internal/store"
	"github.com/opod-io/opod-core/pkg/adminapi"
	"github.com/opod-io/opod-core/pkg/nodeapi"
)

const (
	prefixIndexPerWorker = 200_000 // hashes kept per worker (≈ 3.2 M tokens at block size 16)
	prefixChainCacheSize = 4096    // tokenized prefixes remembered
	// prefixChainCacheHashes bounds the hashes those prefixes hold in all:
	// what 4096 chains of 2,048 tokens at block size 16 held before the chain
	// went deeper (T28.11), so a deep chain costs entries, never memory.
	prefixChainCacheHashes = prefixChainCacheSize * 128
	prefixChainTTL         = 10 * time.Minute
	prefixTokenizeBudget   = 250 * time.Millisecond // a tokenize slower than this is not worth waiting for
	// prefixTokenizeFloor is the fewest leading tokens that decide a match —
	// the depth before T28.11; past it the policy's prefix depth decides
	// (prefixTokenizeMax).
	prefixTokenizeFloor = 2048
	tierGhostCap        = 131_072 // evicted hashes remembered per worker (≈ 2.1 M tokens at block size 16)
)

// tierGhostBuckets are TierGhost's depth buckets, in blocks evicted between a
// block's eviction and its return; the last is the ghost list's cap.
var tierGhostBuckets = []int64{1 << 10, 1 << 12, 1 << 14, 1 << 16, tierGhostCap}

type workerBlocks struct {
	seq       int64
	blockSize int
	held      map[string]*list.Element
	order     *list.List // oldest first
	at        time.Time
	// The worker's host-memory tier (KVBlocks.CPUStored, feature
	// kv_cpu_tier_hits), bounded like the card's set.
	cpuHeld  map[string]*list.Element
	cpuOrder *list.List

	// The ghost list (T26.14): blocks the engine evicted, by eviction number,
	// so a block the engine stores again is a block it computed again — one a
	// host tier holding that many evicted blocks would have handed back.
	evictions  int64
	ghost      map[string]int64 // hash → its eviction number
	ghostOrder *list.List       // ghostEntry, oldest first
	stored     int64            // blocks the engine computed (stored new)
	returns    []int64          // returned blocks per tierGhostBuckets entry
}

type ghostEntry struct {
	hash string
	at   int64
}

func newWorkerBlocks() *workerBlocks {
	return &workerBlocks{held: map[string]*list.Element{}, order: list.New(), cpuHeld: map[string]*list.Element{}, cpuOrder: list.New(),
		ghost: map[string]int64{}, ghostOrder: list.New(), returns: make([]int64, len(tierGhostBuckets))}
}

// evicted remembers an evicted block in the ghost list, oldest out first.
func (w *workerBlocks) evicted(h string) {
	w.evictions++
	w.ghost[h] = w.evictions
	w.ghostOrder.PushBack(ghostEntry{h, w.evictions})
	for w.ghostOrder.Len() > tierGhostCap {
		old := w.ghostOrder.Remove(w.ghostOrder.Front()).(ghostEntry)
		if w.ghost[old.hash] == old.at {
			delete(w.ghost, old.hash)
		}
	}
}

// computed counts a block the engine stored new, and a return when the ghost
// list holds it: the evictions since its own are its depth.
func (w *workerBlocks) computed(h string) {
	w.stored++
	at, ok := w.ghost[h]
	if !ok {
		return
	}
	delete(w.ghost, h)
	depth := w.evictions - at
	for i, max := range tierGhostBuckets {
		if depth <= max {
			w.returns[i]++
			return
		}
	}
}

type prefixIndex struct {
	mu      sync.RWMutex
	workers map[string]*workerBlocks

	chainMu     sync.Mutex
	chains      map[string]chainEntry // prefixCacheKey → chain
	chainHashes int                   // hashes held in chains, bounded by prefixChainCacheHashes
}

type chainEntry struct {
	hashes []string
	until  time.Time
}

func newPrefixIndex() *prefixIndex {
	return &prefixIndex{workers: map[string]*workerBlocks{}, chains: map[string]chainEntry{}}
}

// apply folds one heartbeat's changes into the worker's set.
func (p *prefixIndex) apply(nodeID string, kb *nodeapi.KVBlocks) {
	if kb == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	w := p.workers[nodeID]
	if w == nil || kb.Cleared || (w.seq != 0 && kb.Seq != w.seq+1) {
		// New worker, the worker's own word, or a batch we never saw: start
		// over. The ghost counters are a history, not a set, and survive it.
		fresh := newWorkerBlocks()
		if w != nil {
			fresh.stored, fresh.returns = w.stored, w.returns
		}
		w = fresh
		p.workers[nodeID] = w
	}
	w.seq, w.at = kb.Seq, time.Now()
	if kb.BlockSize > 0 {
		w.blockSize = kb.BlockSize
	}
	for _, h := range kb.Removed {
		if e, ok := w.held[h]; ok {
			w.order.Remove(e)
			delete(w.held, h)
		}
		w.evicted(h)
	}
	for _, h := range kb.Stored {
		if e, ok := w.held[h]; ok {
			w.order.MoveToBack(e)
			continue
		}
		w.computed(h)
		w.held[h] = w.order.PushBack(h)
		if w.order.Len() > prefixIndexPerWorker {
			old := w.order.Front()
			w.order.Remove(old)
			delete(w.held, old.Value.(string))
		}
	}
	for _, h := range kb.CPURemoved {
		if e, ok := w.cpuHeld[h]; ok {
			w.cpuOrder.Remove(e)
			delete(w.cpuHeld, h)
		}
	}
	for _, h := range kb.CPUStored {
		if e, ok := w.cpuHeld[h]; ok {
			w.cpuOrder.MoveToBack(e)
			continue
		}
		w.cpuHeld[h] = w.cpuOrder.PushBack(h)
		if w.cpuOrder.Len() > prefixIndexPerWorker {
			old := w.cpuOrder.Front()
			w.cpuOrder.Remove(old)
			delete(w.cpuHeld, old.Value.(string))
		}
	}
}

// forget drops everything held for a worker (removed, reincarnated, lost).
func (p *prefixIndex) forget(nodeID string) {
	p.mu.Lock()
	delete(p.workers, nodeID)
	p.mu.Unlock()
}

// leading is how many hashes of the chain, from its start, nodeID holds.
func (p *prefixIndex) leading(nodeID string, hashes []string) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	w := p.workers[nodeID]
	if w == nil {
		return 0
	}
	n := 0
	for _, h := range hashes {
		if _, ok := w.held[h]; !ok {
			break
		}
		n++
	}
	return n
}

// leadingAny is how many hashes of the chain, from its start, nodeID holds on
// its card or in its host-memory tier — one copy away (routing.prefixBlockTier
// cpu-lower).
func (p *prefixIndex) leadingAny(nodeID string, hashes []string) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	w := p.workers[nodeID]
	if w == nil {
		return 0
	}
	n := 0
	for _, h := range hashes {
		_, card := w.held[h]
		_, cpu := w.cpuHeld[h]
		if !card && !cpu {
			break
		}
		n++
	}
	return n
}

// state is what /loadz says about the index; nil when nothing reports.
func (p *prefixIndex) state() *adminapi.PrefixIndex {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.workers) == 0 {
		return nil
	}
	out := &adminapi.PrefixIndex{}
	for _, w := range p.workers {
		out.WorkersReporting++
		out.Blocks += len(w.held)
		out.CPUBlocks += len(w.cpuHeld)
	}
	return out
}

// ghostState is /loadz's TierGhost: the reporting workers' ghost counters
// summed; nil when no worker publishes cache events.
func (p *prefixIndex) ghostState() *adminapi.TierGhost {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.workers) == 0 {
		return nil
	}
	out := &adminapi.TierGhost{Returns: make([]adminapi.GhostBucket, len(tierGhostBuckets))}
	for i, max := range tierGhostBuckets {
		out.Returns[i].MaxDepthBlocks = max
	}
	for _, w := range p.workers {
		out.BlockSize = max(out.BlockSize, w.blockSize)
		out.StoredBlocks += w.stored
		for i, n := range w.returns {
			out.Returns[i].Blocks += n
		}
	}
	return out
}

// prune drops the sets of workers that are gone. A replaced pod is a NEW node
// id (n_<pod name>), and the old one simply stops heartbeating — nothing ever
// calls forget for it, so without this its blocks stayed in the index for the
// life of the leader and /loadz counted workers that no longer existed
// (measured: two workers restarted, four reported).
func (s *Server) prunePrefixIndex(ctx context.Context) {
	s.prefix.mu.RLock()
	ids := make([]string, 0, len(s.prefix.workers))
	for id := range s.prefix.workers {
		ids = append(ids, id)
	}
	s.prefix.mu.RUnlock()
	maxAge, now := s.heartbeatMaxAge(), time.Now()
	for _, id := range ids {
		n, err := s.store.Nodes().Get(ctx, id)
		if err != nil {
			continue // unreadable is not gone
		}
		if n == nil || !n.Alive(maxAge, now) {
			s.prefix.forget(id)
		}
	}
}

// reporter is a worker that reports blocks, with the block size it uses, or "".
func (p *prefixIndex) reporter() (string, int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for id, w := range p.workers {
		if w.blockSize > 0 {
			return id, w.blockSize
		}
	}
	return "", 0
}

// liveReporter is a reporting worker that is alive to be asked, pruning as it
// looks: a tokenize sent to a worker that is gone costs the whole budget.
func (s *Server) liveReporter(ctx context.Context) (string, int) {
	for i := 0; i < 4; i++ {
		id, size := s.prefix.reporter()
		if id == "" {
			return "", 0
		}
		if n, err := s.store.Nodes().Get(ctx, id); err == nil && n != nil && n.Alive(s.heartbeatMaxAge(), time.Now()) {
			return id, size
		}
		s.prefix.forget(id)
	}
	return "", 0
}

// blockSizeOf is the block size nodeID reports its blocks in; 0 = it reports
// none.
func (p *prefixIndex) blockSizeOf(nodeID string) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if w := p.workers[nodeID]; w != nil {
		return w.blockSize
	}
	return 0
}

// gangReporter is a ready coordinator of model whose head worker reports
// blocks, with the head's block size (PLAN T26.22). A gang's engine is not the
// head worker's own engine — the leader dials the coordinator directly — so its
// prompt is tokenized there, never through the head's /v1/tokenize.
func (s *Server) gangReporter(ctx context.Context, model string) (store.Shard, int, bool) {
	shards, err := s.store.Shards().GetByModel(ctx, model)
	if err != nil {
		return store.Shard{}, 0, false
	}
	for _, sh := range shards {
		if sh.Role != "coordinator" || sh.Status != "ready" || sh.Address == "" {
			continue
		}
		if size := s.prefix.blockSizeOf(sh.NodeID); size > 0 {
			return sh, size, true
		}
	}
	return store.Shard{}, 0, false
}

// tokenizeOnGang asks a gang's coordinator engine for the prompt's leading
// token ids, through the driver the leader samples it with.
func (s *Server) tokenizeOnGang(ctx context.Context, coord store.Shard, req engines.ChatRequest, limit int) ([]int, bool) {
	eng, _ := s.gangEngine(coord)
	tk, ok := eng.(engines.Tokenizer)
	if !ok {
		return nil, false
	}
	msgs := make([]engines.Message, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, engines.Message{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		msgs = append(msgs, engines.Message{Role: m.Role, Content: m.Content})
	}
	tokens, err := tk.Tokenize(ctx, req.Model, msgs, "", limit)
	return tokens, err == nil && len(tokens) > 0
}

// prefixTokenizeMax is how many leading tokens decide a block match: the
// policy's prefix depth (routing.prefixDepthKiB, router.PrefixDepthKiB) in
// tokens at 4 bytes each — 65,536 at the default 256 KiB — and never fewer
// than prefixTokenizeFloor, so depth 4 tokenizes what it always did.
func (s *Server) prefixTokenizeMax() int {
	depth := router.DefaultPrefixDepthKiB
	if s.router != nil {
		depth = s.router.PrefixDepth()
	}
	return max(prefixTokenizeFloor, depth<<10/4)
}

// blockSeedOf is the seed of the request's block chain (T28.9): the keys the
// engine hashes a prompt's first block with that the leader can rebuild — the
// LoRA adapter's name for a "<model>:<adapter>" variant of the plan's model,
// then the cache salt the router puts on the request (cachesalt.go) — in the
// order vLLM lists them (lora, multimodal, salt). A worker re-hashes from the
// keys its engine published (kvevents), so the two chains meet exactly when
// the engine would share the cache. Keys the leader cannot rebuild (an image)
// make the worker's chain one the leader never asks for: a missed credit,
// never a wrong one. With no plan file there is no base to split an adapter
// from, so an adapter's request is seeded by its salt alone and matches
// nothing: missed again, not wrong.
func (s *Server) blockSeedOf(req engines.ChatRequest) string {
	var keys []any
	if _, base := s.plan.get(); base != "" && strings.HasPrefix(req.Model, base+":") && len(req.Model) > len(base)+1 {
		keys = append(keys, req.Model[len(base)+1:])
	}
	if req.CacheSalt != "" {
		keys = append(keys, req.CacheSalt)
	}
	return nodeapi.BlockSeed(keys)
}

// resolveBlocks is the router's BlockResolver: the request's prompt as a chain
// of block hashes, seeded by the keys the engine keys it by (blockSeedOf). The
// leader has no tokenizer, so it asks an engine that reports blocks — a gang's
// coordinator for a model a reporting gang serves, else a reporting worker's
// engine (nodeapi.PathTokenize) — once per distinct prefix, then from the
// cache — inside a budget: a tokenize that is slow costs the request more
// than a cold cache would.
func (s *Server) resolveBlocks(ctx context.Context, req engines.ChatRequest) []string {
	coord, blockSize, gang := s.gangReporter(ctx, req.Model)
	nodeID := coord.NodeID
	if !gang {
		nodeID, blockSize = s.liveReporter(ctx)
	}
	if nodeID == "" {
		return nil
	}
	seed, limit := s.blockSeedOf(req), s.prefixTokenizeMax()
	key := prefixCacheKey(req, seed, blockSize, limit)
	if key == "" {
		return nil
	}
	now := time.Now()
	s.prefix.chainMu.Lock()
	if e, ok := s.prefix.chains[key]; ok && now.Before(e.until) {
		s.prefix.chainMu.Unlock()
		return e.hashes
	}
	s.prefix.chainMu.Unlock()

	tctx, cancel := context.WithTimeout(ctx, prefixTokenizeBudget)
	defer cancel()
	var tokens []int
	var ok bool
	if gang {
		tokens, ok = s.tokenizeOnGang(tctx, coord, req, limit)
	} else {
		n, err := s.store.Nodes().Get(ctx, nodeID)
		if err != nil || n == nil || n.Address == "" {
			return nil
		}
		tokens, ok = s.tokenizeOn(tctx, n, req, limit)
	}
	if !ok {
		return nil
	}
	hashes := nodeapi.BlockHashes(seed, tokens, blockSize)
	s.prefix.remember(key, hashes, now)
	return hashes
}

// remember caches one prefix's chain, inside both bounds: entries
// (prefixChainCacheSize) and the hashes they hold (prefixChainCacheHashes).
// Past either, expired entries go first, then any — a cache, not a record —
// until both are under three quarters.
func (p *prefixIndex) remember(key string, hashes []string, now time.Time) {
	p.chainMu.Lock()
	defer p.chainMu.Unlock()
	if old, ok := p.chains[key]; ok {
		p.chainHashes -= len(old.hashes)
		delete(p.chains, key)
	}
	over := func(frac int) bool {
		return len(p.chains) >= prefixChainCacheSize*frac/4 || p.chainHashes+len(hashes) > prefixChainCacheHashes*frac/4
	}
	if over(4) {
		for k, e := range p.chains {
			if now.After(e.until) || over(4) {
				delete(p.chains, k)
				p.chainHashes -= len(e.hashes)
			}
			if !over(3) {
				break
			}
		}
	}
	p.chains[key] = chainEntry{hashes: hashes, until: now.Add(prefixChainTTL)}
	p.chainHashes += len(hashes)
}

// prefixCacheKeySeed keys prefixCacheKey's hash. The chain cache lives in
// this process only, so a seed drawn at start is enough.
var prefixCacheKeySeed = maphash.MakeSeed()

// prefixCacheKey keys the chain cache by a hash — the text itself is never a
// key — of everything the chain depends on: the model, the chain's seed (the
// adapter and the caller's salt: two callers with one prompt hold two
// chains), the block size, the tokens asked for, and the prompt's head, as
// far as limit tokens can reach at 32 bytes each. "" = no prompt to key.
func prefixCacheKey(req engines.ChatRequest, seed string, blockSize, limit int) string {
	if req.System == "" && len(req.Messages) == 0 {
		return ""
	}
	var h maphash.Hash
	h.SetSeed(prefixCacheKeySeed)
	room := limit * 32
	field := func(v string) {
		if room <= 0 {
			return
		}
		v = v[:min(len(v), room)]
		room -= len(v)
		var n [binary.MaxVarintLen64]byte
		_, _ = h.Write(n[:binary.PutUvarint(n[:], uint64(len(v)))])
		_, _ = h.WriteString(v)
	}
	_, _ = h.WriteString(fmt.Sprintf("%s\x00%d\x00%d\x00", req.Model, blockSize, limit))
	_, _ = h.WriteString(fmt.Sprintf("%d\x00", len(seed)))
	_, _ = h.WriteString(seed)
	field(req.System)
	for _, m := range req.Messages {
		field(m.Role)
		field(m.Content)
	}
	return strconv.FormatUint(h.Sum64(), 16)
}

// tokenizeOn asks one worker's engine for the prompt's leading token ids.
func (s *Server) tokenizeOn(ctx context.Context, n *store.Node, req engines.ChatRequest, limit int) ([]int, bool) {
	body := nodeapi.TokenizeRequest{Model: req.Model, MaxTokens: limit}
	if req.System != "" {
		body.Messages = append(body.Messages, nodeapi.TokenizeMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		body.Messages = append(body.Messages, nodeapi.TokenizeMessage{Role: m.Role, Content: m.Content})
	}
	raw, _ := json.Marshal(body)
	addr := n.Address
	if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
		addr = "http://" + addr
	}
	hreq, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(addr, "/")+nodeapi.PathTokenize, bytes.NewReader(raw))
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Authorization", "Bearer "+n.WorkerToken)
	auth.SignRequest(hreq, n.ID, n.WorkerToken)
	resp, err := s.workerHTTP().Do(hreq)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, false
	}
	var out nodeapi.TokenizeResponse
	// A token id is at most 7 characters of JSON with its comma; 16 bytes
	// each is room to spare, and never less than the 1 MiB it always had.
	if err := json.NewDecoder(io.LimitReader(resp.Body, max(1<<20, int64(limit)*16))).Decode(&out); err != nil {
		return nil, false
	}
	return out.Tokens, len(out.Tokens) > 0
}
