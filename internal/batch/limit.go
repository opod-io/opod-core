package batch

import (
	"context"
	"strconv"
	"sync"
)

// The in-flight limit, and how --concurrency auto moves it (PLAN T34.1).
//
// The dispatcher takes a slot before it hands a line to a worker and the
// worker gives it back after the line's answer, so "a line handed out is a
// line sent" holds whatever the limit does. With a fixed --concurrency the
// limit never moves. With auto it starts at Options.Concurrency and follows
// the leader's X-Opod-Admission-Cap (feature "admission_cap_header"): the
// requests the batch's class may hold at once, plus a quarter more (at most
// maxMargin), so a request is already waiting in the leader when a slot frees
// and the slot is granted with no round trip to here. The margin is bounded
// because the leader's hold is shared by every class (admission.maxHeld,
// default 64): a margin that filled it would get a second batch's requests
// shed on arrival, since nothing below sheddable can make room for them. The leader ranks classes, so the extra
// never delays a higher class: one arriving to a full hold sheds a waiting
// batch request (retried, see send), never the reverse. A server that sends
// no header leaves the limit where it started.
// maxMargin bounds the requests an auto limit keeps waiting in the leader
// beyond its cap: enough to cover one round trip, a quarter of the default hold.
const maxMargin = 16

type limiter struct {
	mu    sync.Mutex
	limit int
	inUse int
	max   int
	auto  bool
	wake  chan struct{} // a slot freed or the limit rose
}

func newLimiter(start, most int, auto bool) *limiter {
	return &limiter{limit: min(start, most), max: most, auto: auto, wake: make(chan struct{}, 1)}
}

// acquire waits for a free slot. False = done closed first.
func (l *limiter) acquire(ctx context.Context, done <-chan struct{}) bool {
	for {
		l.mu.Lock()
		if l.inUse < l.limit {
			l.inUse++
			l.mu.Unlock()
			return true
		}
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return false
		case <-done:
			return false
		case <-l.wake:
		}
	}
}

func (l *limiter) release() {
	l.mu.Lock()
	l.inUse--
	l.mu.Unlock()
	l.signal()
}

// observe moves an auto limit to what the leader's cap header says; a fixed
// limit, or an answer with no valid header, leaves it alone.
func (l *limiter) observe(header string) {
	if !l.auto || header == "" {
		return
	}
	n, err := strconv.Atoi(header)
	if err != nil || n < 1 {
		return
	}
	want := min(n+min((n+3)/4, maxMargin), l.max)
	l.mu.Lock()
	rose := want > l.limit
	l.limit = want
	l.mu.Unlock()
	if rose {
		l.signal()
	}
}

func (l *limiter) signal() {
	select {
	case l.wake <- struct{}{}:
	default:
	}
}
