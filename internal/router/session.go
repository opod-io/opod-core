package router

// A conversation's identity for routing (ADR-109, PLAN T28.3). The gateway
// reads it from X-Opod-Session, else the OpenAI `user` field, and attaches it
// with WithSession; the pin is then keyed by the conversation, not the key, so
// two conversations under one key land where each one's cache is. A request
// whose answer named a worker (the signed route token) is routed there first
// by WithRoutedNode. Both are preferences: a pin never lands a request on a
// worker that is saturated or has no free slot.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// MaxSessionIDBytes bounds X-Opod-Session; a longer one is not a session.
const MaxSessionIDBytes = 128

type sessionKey struct{}
type routedNodeKey struct{}

// sessionPin is a request's conversation as the pin table keys it, and how
// long the pin lasts (0 = the router's sticky TTL).
type sessionPin struct {
	id  string // hashed: the caller's id is never kept
	ttl time.Duration
}

// HashSession is the form a session id is kept in: 128 bits of SHA-256, hex.
func HashSession(id string) string {
	sum := sha256.Sum256([]byte("opod-session/v1\x00" + id))
	return hex.EncodeToString(sum[:16])
}

// WithSession attaches the request's conversation. "" or an id over
// MaxSessionIDBytes attaches nothing: the request pins per key, as before.
func WithSession(ctx context.Context, id string, ttl time.Duration) context.Context {
	if id == "" || len(id) > MaxSessionIDBytes {
		return ctx
	}
	return context.WithValue(ctx, sessionKey{}, sessionPin{id: HashSession(id), ttl: ttl})
}

// SessionFrom is the request's hashed session id, "" when it has none.
func SessionFrom(ctx context.Context) string {
	p, _ := ctx.Value(sessionKey{}).(sessionPin)
	return p.id
}

// WithRoutedNode names the worker a verified route token pointed at.
func WithRoutedNode(ctx context.Context, nodeID string) context.Context {
	if nodeID == "" {
		return ctx
	}
	return context.WithValue(ctx, routedNodeKey{}, nodeID)
}

func routedNodeFrom(ctx context.Context) string {
	n, _ := ctx.Value(routedNodeKey{}).(string)
	return n
}

// RoutedNodeFrom is the worker a verified route token named ("" = none).
func RoutedNodeFrom(ctx context.Context) string { return routedNodeFrom(ctx) }

// pinKey is whose pin this request reads and writes, and for how long it
// lasts: the conversation when the request has one, else the caller.
func (r *Router) pinKey(ctx context.Context, model string) (string, time.Duration) {
	if p, ok := ctx.Value(sessionKey{}).(sessionPin); ok && p.id != "" {
		ttl := p.ttl
		if ttl <= 0 {
			ttl = r.StickyTTL()
		}
		return "session:" + p.id + "|" + model, ttl
	}
	if u := userIDFor(ctx); u != "" {
		return u + "|" + model, r.StickyTTL()
	}
	return "", 0
}

// pinHonoured reports whether a pin may put this request on node: never on a
// worker whose slots are all in flight, nor one at or above the KV saturation
// line — a pin never beats load (ADR-109 §4). The request then routes by rank,
// and the pin moves to where it lands.
func (r *Router) pinHonoured(node string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	saturated, _ := r.loadRank(node)
	return !saturated
}
