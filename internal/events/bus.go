// Package events is a small in-process publish/subscribe bus for the
// admin dashboard's live updates. Producers (the model add/remove path,
// node heartbeat handler, usage recorder, etc.) call Publish; the
// /admin/v1/events SSE endpoint subscribes a channel per connected
// dashboard.
//
// Design choices:
//
//   - Subscribers get a buffered channel; if they fall behind, the
//     event being published is dropped for them (we drop the NEWEST
//     rather than block the producer) so a wedged dashboard tab can't
//     slow the gateway.
//   - The bus is process-local. We don't need multi-leader fan-out
//     today; if that ever changes the producers stay the same.
//   - Event payloads are tiny strings ("models", "nodes", "usage",
//     "audit") — the frontend just re-fetches the corresponding tab on
//     receipt. We don't ship the diff over the wire because the admin
//     API is already fast and the diff plumbing isn't worth the
//     complexity for a 19-tab dashboard.
package events

import (
	"sync"
)

// Topic is the kind of event. Subscribers see every topic; if they
// only care about a subset they filter on the receive side.
type Topic string

const (
	TopicModels Topic = "models"
	TopicNodes  Topic = "nodes"
	TopicUsage  Topic = "usage"
	TopicAudit  Topic = "audit"
	TopicShards Topic = "shards"
)

// Event is a single bus message. Producers call Publish to fan one of
// these out to every active subscriber.
type Event struct {
	Topic Topic  `json:"topic"`
	ID    string `json:"id,omitempty"` // optional: the model id / node id / etc.
}

// Bus is a process-local pub/sub. Zero value is not usable; call New.
type Bus struct {
	mu   sync.RWMutex
	subs map[chan Event]struct{}
}

// New constructs an empty Bus.
func New() *Bus {
	return &Bus{subs: map[chan Event]struct{}{}}
}

// Publish fans an event out to every subscriber. Non-blocking — if a
// subscriber's buffer is full we drop the event for that subscriber
// rather than stalling the publisher.
func (b *Bus) Publish(ev Event) {
	if b == nil {
		return
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.subs {
		select {
		case ch <- ev:
		default:
			// Buffer full → drop. The frontend's slow polling fallback
			// will catch up on the next tick.
		}
	}
}

// Subscribe registers a new subscriber and returns the receive channel.
// Callers must call the returned cancel func when they're done (e.g. on
// SSE client disconnect) so the bus can drop their slot. cancel is
// idempotent — calling it twice won't double-close the channel.
func (b *Bus) Subscribe(buf int) (<-chan Event, func()) {
	if buf <= 0 {
		buf = 16
	}
	ch := make(chan Event, buf)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, ch)
			b.mu.Unlock()
			close(ch)
		})
	}
	return ch, cancel
}

// Len reports the current subscriber count. Used by admin tooling
// + tests; not part of the public hot path.
func (b *Bus) Len() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}
