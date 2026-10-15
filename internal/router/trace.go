package router

import (
	"context"
	"errors"
	"sync"
)

// routeTrace is a per-request slot the router fills with the node it
// dispatched to, so the usage record can attribute the request to a worker
// (and the leader's admission gate can see which worker holds it).
// The API handler installs it with WithTrace; pick()'s callers NoteNode
// on every dispatch (a fallback to another worker overwrites), and
// recordUsage reads it back with NodeFrom. Absent slot = no-op everywhere.
type routeTrace struct {
	mu   sync.Mutex
	node string
}

type traceKey struct{}

// WithTrace returns ctx carrying a route trace: the one ctx already carries,
// else a new empty one. Reusing it lets a caller that installed the trace
// earlier read the same slot — the leader's admission gate does, to see the
// worker each request it granted went to.
func WithTrace(ctx context.Context) context.Context {
	if t, ok := ctx.Value(traceKey{}).(*routeTrace); ok && t != nil {
		return ctx
	}
	return context.WithValue(ctx, traceKey{}, &routeTrace{})
}

// NoteNode records the worker a dispatch went to, for the usage row. Called
// on every attempt (a fallback overwrites); a no-op when no trace is installed.
func NoteNode(ctx context.Context, nodeID string) {
	if t, ok := ctx.Value(traceKey{}).(*routeTrace); ok && t != nil {
		t.mu.Lock()
		t.node = nodeID
		t.mu.Unlock()
	}
}

// attemptError is the error of one dispatch attempt with the worker or gang
// (router key) that gave it. The router returns the FIRST worker's error when
// every attempt fails, while the trace names the LAST worker tried; a message
// built from both named one worker and quoted another's error. FailedNode
// reads the name back. The error's text and type are the wrapped error's.
type attemptError struct {
	node string
	err  error
}

func (e *attemptError) Error() string { return e.err.Error() }
func (e *attemptError) Unwrap() error { return e.err }

// failedOn tags err with the node of the attempt that produced it.
func failedOn(node string, err error) error {
	if err == nil || node == "" {
		return err
	}
	return &attemptError{node: node, err: err}
}

// FailedNode is the worker or gang whose error err is, when the router tagged
// it, else fallback (typically NodeFrom: the last worker tried). A message
// that quotes err names this node, so the name and the error agree.
func FailedNode(err error, fallback string) string {
	var ae *attemptError
	if errors.As(err, &ae) {
		return ae.node
	}
	return fallback
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
