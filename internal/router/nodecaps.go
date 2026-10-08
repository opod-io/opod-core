package router

import (
	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/store"
)

// nodeCaps is what the pick path reads off a node's capabilities blob: the
// role and plan revision it registered. raw is the blob the pair was parsed
// from — the blob is written once at registration, so the pair is valid for
// exactly as long as the blob is unchanged.
type nodeCaps struct {
	raw      string
	role     string
	revision int
	// kvPeer: the worker's engine keeps a KV cache tier its siblings read
	// from (Capabilities.KVTier "peer", ADR-089) — prefixblocks.go scores a
	// sibling's blocks as a peer hit for it.
	kvPeer bool
}

// capsOf returns a node's parsed capabilities, parsing the blob only when it
// is one this router has not seen for that node. Before this, roleOf and
// revisionOf each json.Unmarshal'd the whole hardware_json and each ran twice
// per candidate (decodeOnly and pickRevisionGroup both loop twice), so a pick
// parsed the blob four times per candidate per request — measured at 109 µs
// for 8 workers, comparable to every SQLite query on the request path put
// together (PLAN T15.6). In-memory only: the pair is derived from a column the
// store already holds, and InvalidateNode drops it with the node.
func (r *Router) capsOf(n *store.Node) nodeCaps {
	if n == nil || n.HardwareJSON == "" {
		return nodeCaps{}
	}
	r.mu.RLock()
	c, ok := r.caps[n.ID]
	r.mu.RUnlock()
	if ok && c.raw == n.HardwareJSON {
		return c
	}
	c = nodeCaps{raw: n.HardwareJSON, role: roleOf(n), revision: revisionOf(n), kvPeer: kvTierOf(n) == KVTierPeer}
	r.mu.Lock()
	if r.caps == nil {
		r.caps = map[string]nodeCaps{}
	}
	r.caps[n.ID] = c
	r.mu.Unlock()
	return c
}

// idleCloser is what an evicted engine is asked for: the pooled sockets it
// holds, released now rather than at the transport's idle timeout (T15.16).
type idleCloser interface{ CloseIdleConnections() }

// releaseEngine drops what an engine the router will never dial again still
// holds. Safe on any engine: one with no pool is simply not asked.
func releaseEngine(e engines.Engine) {
	if c, ok := e.(idleCloser); ok {
		c.CloseIdleConnections()
	}
}

// HoldsNode reports whether any per-node state on the router still names the
// node: what InvalidateNode is expected to leave empty. For tests.
func (r *Router) HoldsNode(nodeID string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if _, ok := r.remotes[nodeID]; ok {
		return true
	}
	if _, ok := r.caps[nodeID]; ok {
		return true
	}
	if _, ok := r.staleWarned[nodeID]; ok {
		return true
	}
	if _, ok := r.inflight[nodeID]; ok {
		return true
	}
	if _, ok := r.cooldowns[nodeID]; ok {
		return true
	}
	if _, ok := r.failures[nodeID]; ok {
		return true
	}
	return false
}
