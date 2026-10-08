package api

import (
	"context"
	"time"
)

// arrivalKey carries when a request reached this process, before anything
// made it wait.
type arrivalKey struct{}

// WithArrival stamps the moment a request arrived. The leader stamps it before
// its admission gate (ADR-091), so the time to first token and the latency a
// usage row reports include the wait for a worker slot — the wait a caller
// sees. Measured from after the gate, both hid exactly the queueing the gate
// moved out of the engine and into the leader.
func WithArrival(ctx context.Context, at time.Time) context.Context {
	return context.WithValue(ctx, arrivalKey{}, at)
}

// arrivalOf is the stamped arrival, or now for a request nobody stamped (a
// handler served without the leader's dispatch in front of it).
func arrivalOf(ctx context.Context) time.Time {
	if at, ok := ctx.Value(arrivalKey{}).(time.Time); ok && !at.IsZero() {
		return at
	}
	return time.Now()
}
