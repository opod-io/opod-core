package router

// The prefill/decode handoff (PLAN T16.9, feature pd_handoff). roles.go keeps
// generation off a prefill half; this is what puts the prefill half to work:
// for a request picked onto a DECODE worker, the leader first sends the
// prompt to a PREFILL worker of the same model as one non-streamed call with
// max_tokens 1 and kv_transfer_params {do_remote_decode: true}, then hands the
// kv_transfer_params that worker's engine answered with to the decode call,
// whose engine pulls the prompt's KV cache instead of computing it. Two plain
// calls on the OpenAI wire the leader already speaks; no engine API of ours.
//
// The decode worker is picked FIRST (pick, then its in-flight slot counted by
// the caller), and only then is the prompt prefilled: a request is never
// prefilled and parked waiting for a decoder.
//
// It never makes a request fail that would have been served unsplit. Anything
// that stands between the request and a correct split — no prefill worker
// taking requests, a repetition penalty in force (vLLM #63693: the decode
// answer differs from a complete server's), a penalty nobody can read, an
// engine whose handoff is not built, a prefill step that fails — sends the
// request to its decode worker WHOLE, which computes the prompt itself. Each
// such outcome is counted (opod_pd_handoff_total) with its reason.

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/metrics"
	"github.com/opod-io/opod/internal/store"
)

// prefillStep is what a vLLM prefill engine is asked for: compute the prompt,
// keep its blocks for a remote decoder, answer with where they are.
var prefillStep = json.RawMessage(`{"do_remote_decode":true,"do_remote_prefill":false,"remote_engine_id":null,"remote_block_ids":null,"remote_host":null,"remote_port":null}`)

// prefillTries is how many prefill workers one request asks before it is
// served unsplit.
const prefillTries = 2

// handoffEngines are the engines whose handoff the leader conducts: vLLM's
// NixlConnector, sequential (prefill, then decode). SGLang's runs both calls at
// once on a shared bootstrap room and is not built (PLAN T16.9).
var handoffEngines = map[string]bool{"vllm": true}

// samplingDefaultsTTL bounds how long a worker's answer is trusted. The key is
// the worker's boot id as well (a restarted worker is a new one, ADR-042), so
// this only covers a model reloaded inside one process.
const samplingDefaultsTTL = 10 * time.Minute

// samplingUnknownTTL is how long "the worker cannot say" is kept: briefly,
// because what makes it known arrives without restarting the worker — a
// rollout's prefetch writes the snapshot the worker reads (design-partner
// cell, 2026-10-26: an unknown cached for the full TTL kept a pair from
// splitting for ten minutes after its snapshot landed).
const samplingUnknownTTL = 30 * time.Second

type samplingKey struct{ node, boot, model string }

type samplingEntry struct {
	known   bool
	penalty float64
	at      time.Time
}

// samplingCache is the leader's memory of each decode worker's model defaults.
type samplingCache struct {
	mu sync.Mutex
	m  map[samplingKey]samplingEntry
}

func (c *samplingCache) get(k samplingKey, now time.Time) (samplingEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	ttl := samplingDefaultsTTL
	if !e.known {
		ttl = samplingUnknownTTL
	}
	if !ok || now.Sub(e.at) > ttl {
		return samplingEntry{}, false
	}
	return e, true
}

func (c *samplingCache) put(k samplingKey, e samplingEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[samplingKey]samplingEntry{}
	}
	c.m[k] = e
}

// samplingDefaulter is a worker client that can ask for the served model's
// sampling defaults (openaicompat.Client.SamplingDefaults).
type samplingDefaulter interface {
	SamplingDefaults(ctx context.Context, model string) (known bool, repetitionPenalty float64, err error)
}

// prefillHandoff runs the prefill step for a request already picked onto
// decodeNode and returns the kv_transfer_params for its decode call, or false
// when the request is to be served whole (the reason is counted).
func (r *Router) prefillHandoff(ctx context.Context, model, decodeNode string, req engines.ChatRequest) (json.RawMessage, bool) {
	placements, err := r.store.Placements().GetByModel(ctx, model)
	if err != nil {
		return nil, false // no store, no roles: nothing to split by
	}
	workers, nodes := r.takingRequests(ctx, model, placements)
	workers = withoutSkipped(ctx, workers)
	dn, ok := nodes[decodeNode]
	if !ok || r.capsOf(dn).role != "decode" {
		return nil, false // a complete server was picked: nothing to hand off
	}
	var prefill []store.Placement
	for _, w := range workers {
		if n, ok := nodes[w.NodeID]; ok && r.capsOf(n).role == "prefill" {
			prefill = append(prefill, w)
		}
	}
	if len(prefill) == 0 {
		metrics.ObservePDHandoff("no-prefill")
		return nil, false
	}
	if !handoffEngines[r.capsOf(dn).engine] {
		metrics.ObservePDHandoff("engine")
		return nil, false
	}
	if why := r.penaltyBlocksSplit(ctx, dn, model, req); why != "" {
		metrics.ObservePDHandoff(why)
		return nil, false
	}

	r.mu.RLock()
	type ranked struct {
		saturated bool
		score     float64
	}
	rank := make(map[string]ranked, len(prefill))
	for _, w := range prefill {
		sat, sc := r.loadRank(w.NodeID)
		rank[w.NodeID] = ranked{sat, sc}
	}
	r.mu.RUnlock()
	sort.SliceStable(prefill, func(i, j int) bool {
		a, b := rank[prefill[i].NodeID], rank[prefill[j].NodeID]
		if a.saturated != b.saturated {
			return !a.saturated
		}
		return a.score < b.score
	})

	step := req
	one := 1
	step.MaxTokens = &one
	step.Stream = false
	step.KVTransfer = prefillStep
	if _, set := req.Extra["max_completion_tokens"]; set {
		step.Extra = make(map[string]json.RawMessage, len(req.Extra))
		for k, v := range req.Extra {
			step.Extra[k] = v
		}
		step.Extra["max_completion_tokens"] = json.RawMessage("1")
	}
	for i, w := range prefill {
		if i == prefillTries {
			break
		}
		n := nodes[w.NodeID]
		eng := r.getOrCreateRemote(n.ID, n.Address, n.WorkerToken, r.capsOf(n).engine)
		once, ok := eng.(engines.OneShotChatter)
		if !ok {
			continue
		}
		r.incInflight(n.ID, model)
		ans, err := once.ChatOnce(ctx, step)
		r.decInflight(n.ID, model)
		if err == nil && engines.HasJSON(ans.KVTransfer) {
			metrics.ObservePDHandoff("split")
			return ans.KVTransfer, true
		}
		if r.log != nil {
			r.log.Warn("prefill step failed; trying the next prefill worker or serving whole", "model", model, "node", n.ID, "err", err, "kv_transfer_params", engines.HasJSON(ans.KVTransfer))
		}
	}
	metrics.ObservePDHandoff("prefill-failed")
	return nil, false
}

// penaltyBlocksSplit says why a request may not be split because of its
// repetition penalty, or "" when it may. The penalty in force is the
// request's own, else the decode worker's model default (generation_config),
// which only that worker can read. A penalty that is not exactly 1 — or one
// nobody can read — blocks the split: vLLM's decode answers differently after
// a remote prefill whenever a repetition penalty is on (vLLM #63693; PLAN
// T16.17), and an answer that differs from a complete server's is never
// served.
func (r *Router) penaltyBlocksSplit(ctx context.Context, dn *store.Node, model string, req engines.ChatRequest) string {
	if raw, set := req.Extra["repetition_penalty"]; set {
		p, err := strconv.ParseFloat(string(raw), 64)
		if err != nil {
			return "penalty-unknown"
		}
		if p != 1 {
			return "penalty"
		}
		return ""
	}
	key := samplingKey{node: dn.ID, boot: dn.BootID, model: model}
	e, ok := r.sampling.get(key, time.Now())
	if !ok {
		eng := r.getOrCreateRemote(dn.ID, dn.Address, dn.WorkerToken, r.capsOf(dn).engine)
		ask, can := eng.(samplingDefaulter)
		if !can {
			return "penalty-unknown"
		}
		known, penalty, err := ask.SamplingDefaults(ctx, model)
		if err != nil {
			return "penalty-unknown" // not cached: the next request asks again
		}
		e = samplingEntry{known: known, penalty: penalty, at: time.Now()}
		r.sampling.put(key, e)
	}
	switch {
	case !e.known:
		return "penalty-unknown"
	case e.penalty != 1:
		return "penalty"
	}
	return ""
}
