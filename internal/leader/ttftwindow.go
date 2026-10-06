package leader

// Time to first token on /loadz (PLAN T16.3, core half): the leader measures
// TTFT on every streamed answer (R15.13, api.recordUsageTTFT) and, until this,
// only wrote it into the usage row — /loadz's ttft_p50_ms / ttft_p95_ms stayed
// empty, so a scaler with a latency budget had nothing to read.
//
// The window is FIXED memory and time-bounded: a ring of the last
// ttftWindowCap streamed answers, of which only those inside ttftWindowSpan
// count. "The last minute, or the last 4096 streamed answers, whichever is
// smaller" — at more than ~68 streamed answers a second the window is shorter
// than a minute, which is still the recent past a scaler wants. Nothing here
// grows with traffic (W15).

import (
	"context"
	"math"
	"slices"
	"sync"
	"time"
)

const (
	ttftWindowSpan = time.Minute
	ttftWindowCap  = 4096
)

type ttftWindow struct {
	mu   sync.Mutex
	at   [ttftWindowCap]int64 // unix nanoseconds of each sample
	ms   [ttftWindowCap]int64 // the sample, in milliseconds (≥ 1)
	next int                  // the slot the next sample overwrites
	n    int                  // slots in use, ≤ ttftWindowCap
}

// add records one streamed answer's TTFT. A sub-millisecond one counts as 1 ms,
// because 0 on the wire means "not measured".
func (w *ttftWindow) add(now time.Time, d time.Duration) {
	if d <= 0 {
		return
	}
	ms := max(d.Milliseconds(), 1)
	w.mu.Lock()
	w.at[w.next], w.ms[w.next] = now.UnixNano(), ms
	w.next = (w.next + 1) % ttftWindowCap
	if w.n < ttftWindowCap {
		w.n++
	}
	w.mu.Unlock()
}

// percentiles returns the nearest-rank p50 and p95 of the samples inside the
// span, or zeros when there are none.
func (w *ttftWindow) percentiles(now time.Time) (p50, p95 int64) {
	cutoff := now.Add(-ttftWindowSpan).UnixNano()
	w.mu.Lock()
	vals := make([]int64, 0, w.n)
	for i := 0; i < w.n; i++ {
		if w.at[i] > cutoff {
			vals = append(vals, w.ms[i])
		}
	}
	w.mu.Unlock()
	if len(vals) == 0 {
		return 0, 0
	}
	slices.Sort(vals)
	return nearestRank(vals, 0.50), nearestRank(vals, 0.95)
}

// nearestRank is the smallest value at or above fraction p of sorted.
func nearestRank(sorted []int64, p float64) int64 {
	i := int(math.Ceil(p*float64(len(sorted)))) - 1
	return sorted[max(i, 0)]
}

// probeCtxKey marks a request that asked not to be counted as demand
// (ProbeHeader); trackLoad sets it so the TTFT window can skip it too — a
// prover's first token must not move a number an autoscaler acts on.
type probeCtxKey struct{}

// observeTTFT is the api.Handler's OnTTFT: every streamed answer that is not a
// probe joins the window.
func (s *Server) observeTTFT(ctx context.Context, d time.Duration) {
	if probe, _ := ctx.Value(probeCtxKey{}).(bool); probe {
		return
	}
	s.ttft.add(time.Now(), d)
}
