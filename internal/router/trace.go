package router

import (
	"context"
	"sync"
)

// routeTrace is a per-request slot the router fills with the node it
// dispatched to, so the usage record can attribute the request to a worker.
// The API handler installs it with WithTrace; pick()'s callers note the node
// on every dispatch (a fallback to another worker overwrites), and
// recordUsage reads it back with NodeFrom. Absent slot = no-op everywhere.
type routeTrace struct {
	mu   sync.Mutex
	node string
}

type traceKey struct{}

// WithTrace returns ctx carrying an empty route trace.
func WithTrace(ctx context.Context) context.Context {
	return context.WithValue(ctx, traceKey{}, &routeTrace{})
}

func noteNode(ctx context.Context, nodeID string) {
	if t, ok := ctx.Value(traceKey{}).(*routeTrace); ok && t != nil {
		t.mu.Lock()
		t.node = nodeID
		t.mu.Unlock()
	}
}

// NodeFrom is the node id the router last dispatched this request to
// ("" when the request never reached a worker or no trace was installed).
func NodeFrom(ctx context.Context) string {
	if t, ok := ctx.Value(traceKey{}).(*routeTrace); ok && t != nil {
		t.mu.Lock()
		defer t.mu.Unlock()
		return t.node
	}
	return ""
}
