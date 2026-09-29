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
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/opod-io/opod-sdk/adminapi"
	"github.com/opod-io/opod-sdk/nodeapi"

	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/store"
)

const (
	prefixIndexPerWorker = 200_000 // hashes kept per worker (≈ 3.2 M tokens at block size 16)
	prefixChainCacheSize = 4096    // tokenized prefixes remembered
	prefixChainTTL       = 10 * time.Minute
	prefixTokenizeBudget = 250 * time.Millisecond // a tokenize slower than this is not worth waiting for
	prefixTokenizeMax    = 2048                   // leading tokens that decide a match
)

type workerBlocks struct {
	seq       int64
	blockSize int
	held      map[string]*list.Element
	order     *list.List // oldest first
	at        time.Time
}

type prefixIndex struct {
	mu      sync.RWMutex
	workers map[string]*workerBlocks

	chainMu sync.Mutex
	chains  map[string]chainEntry // prefix key|model|block size → chain
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
		// New worker, the worker's own word, or a batch we never saw: start over.
		w = &workerBlocks{held: map[string]*list.Element{}, order: list.New()}
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
	}
	for _, h := range kb.Stored {
		if e, ok := w.held[h]; ok {
			w.order.MoveToBack(e)
			continue
		}
		w.held[h] = w.order.PushBack(h)
		if w.order.Len() > prefixIndexPerWorker {
			old := w.order.Front()
			w.order.Remove(old)
			delete(w.held, old.Value.(string))
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
	}
	return out
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

// resolveBlocks is the router's BlockResolver: the request's prompt as a chain
// of block hashes. The leader has no tokenizer, so it asks a reporting
// worker's engine (nodeapi.PathTokenize) — once per distinct prefix, then from
// the cache — inside a budget: a tokenize that is slow costs the request more
// than a cold cache would.
func (s *Server) resolveBlocks(ctx context.Context, req engines.ChatRequest) []string {
	nodeID, blockSize := s.prefix.reporter()
	if nodeID == "" {
		return nil
	}
	key := prefixCacheKey(req, blockSize)
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

	n, err := s.store.Nodes().Get(ctx, nodeID)
	if err != nil || n == nil || n.Address == "" {
		return nil
	}
	tctx, cancel := context.WithTimeout(ctx, prefixTokenizeBudget)
	defer cancel()
	tokens, ok := s.tokenizeOn(tctx, n, req)
	if !ok {
		return nil
	}
	hashes := nodeapi.BlockHashes(tokens, blockSize)
	s.prefix.chainMu.Lock()
	if len(s.prefix.chains) >= prefixChainCacheSize {
		for k, e := range s.prefix.chains { // expired first, then anything: a cache, not a record
			if now.After(e.until) || len(s.prefix.chains) >= prefixChainCacheSize {
				delete(s.prefix.chains, k)
			}
			if len(s.prefix.chains) < prefixChainCacheSize*3/4 {
				break
			}
		}
	}
	s.prefix.chains[key] = chainEntry{hashes: hashes, until: now.Add(prefixChainTTL)}
	s.prefix.chainMu.Unlock()
	return hashes
}

// prefixCacheKey keys the chain cache by a hash of the prompt's head — the
// text itself is never a key — the model and the block size.
func prefixCacheKey(req engines.ChatRequest, blockSize int) string {
	var b strings.Builder
	b.WriteString(req.Model)
	b.WriteByte(0)
	b.WriteString(req.System)
	for _, m := range req.Messages {
		b.WriteByte(0)
		b.WriteString(m.Role)
		b.WriteByte(0)
		b.WriteString(m.Content)
		if b.Len() > 64<<10 {
			break // the head decides; prefixTokenizeMax bounds what is compared anyway
		}
	}
	if b.Len() <= len(req.Model)+1 {
		return ""
	}
	return nodeapi.BlockHash("", []int{blockSize}) + nodeapi.BlockHash(b.String(), nil)
}

// tokenizeOn asks one worker's engine for the prompt's leading token ids.
func (s *Server) tokenizeOn(ctx context.Context, n *store.Node, req engines.ChatRequest) ([]int, bool) {
	body := nodeapi.TokenizeRequest{Model: req.Model, MaxTokens: prefixTokenizeMax}
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
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, false
	}
	return out.Tokens, len(out.Tokens) > 0
}
