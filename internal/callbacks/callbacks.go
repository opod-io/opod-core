// Package callbacks ships usage / audit / fallback events to external
// observability sinks (generic webhooks, Langfuse, etc.). Sinks run on
// their own goroutines with bounded queues so a slow receiver can't
// stall the inference path; overflow events are dropped and counted.
package callbacks

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"
)

// Event is a single observability payload. Kind is one of
// "usage" | "audit" | "fallback"; the same struct is reused across
// kinds because sinks discriminate on Kind and inspect Payload.
type Event struct {
	Kind      string         `json:"kind"`
	Timestamp time.Time      `json:"ts"`
	Payload   map[string]any `json:"payload"`
}

// Sink is the small interface every driver implements. Send is
// non-blocking — drivers buffer internally and a queue-full event is
// dropped (and counted) rather than blocking the caller.
type Sink interface {
	Name() string                // unique identifier, used in metrics + admin test endpoint
	Subscribes(kind string) bool // false if the sink opted out of this Event Kind
	Send(ctx context.Context, e Event)
	// Close drains the queue or aborts, returning when the worker has
	// stopped. Called by the server on shutdown.
	Close(ctx context.Context) error
}

// Dispatcher fans out events to every configured sink. Hot path
// callers (recordUsage, audit, fallback log) call Publish with the
// event; the dispatcher hands it to each subscribed sink and returns
// immediately.
type Dispatcher struct {
	sinks []Sink
	log   *slog.Logger
	mu    sync.RWMutex
}

// NewDispatcher returns a dispatcher with the given sinks. nil sinks
// are dropped silently (lets the config builder be lazy).
func NewDispatcher(log *slog.Logger, sinks ...Sink) *Dispatcher {
	out := &Dispatcher{log: log}
	for _, s := range sinks {
		if s != nil {
			out.sinks = append(out.sinks, s)
		}
	}
	return out
}

// Publish hands the event to every subscribed sink. Cheap on the hot
// path: each Send is non-blocking.
func (d *Dispatcher) Publish(ctx context.Context, e Event) {
	if d == nil {
		return
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, s := range d.sinks {
		if s.Subscribes(e.Kind) {
			s.Send(ctx, e)
		}
	}
}

// Sinks returns the dispatcher's configured sinks (used by the admin
// test endpoint to look up a sink by name).
func (d *Dispatcher) Sinks() []Sink {
	if d == nil {
		return nil
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]Sink, len(d.sinks))
	copy(out, d.sinks)
	return out
}

// Close stops every sink. Best-effort: a sink that fails to close in
// time gets its goroutine leaked, but the process is shutting down
// anyway.
func (d *Dispatcher) Close(ctx context.Context) error {
	if d == nil {
		return nil
	}
	for _, s := range d.sinks {
		_ = s.Close(ctx)
	}
	return nil
}

// MustMarshal is a small helper that JSON-encodes the payload or
// returns an empty object on failure. Sinks use it so a misshapen
// payload (e.g. a circular reference) becomes a "{}" rather than
// crashing the worker.
func MustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}
