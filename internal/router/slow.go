package router

// Per-worker latency outliers (PLAN T28.12, ADR-122 §4; feature
// "worker_slow").
//
// A card that throttles, a link that trained down, a noisy neighbour: the
// worker passes every health check and every request on it is slow (a gray
// failure). The router measures what it serves — never a control-plane probe
// (§11) — and, per model, compares each worker with its siblings:
//
//   - TTFT per 1,000 uncached prompt tokens (the T28.10 estimate, at least
//     slowTokensFloor: below that TTFT is the fixed cost of a step, not
//     prefill), on the normal dispatch path, for a request served whole (a
//     decode half that received its KV from a prefill worker has no prefill
//     of its own to time);
//   - the mean gap between streamed text events of one answer, at least
//     slowGapFloor.
//
// Each is a window of the last slowWindow samples per (model, worker), none
// older than slowSampleMaxAge. A worker whose median of either exceeds
// routing.slowFactor × the median of its siblings' medians — the worker and
// at least one sibling each holding slowWindow fresh samples — is slow, and
// its factor (own median ÷ siblings') is added to its rank score, as that
// many requests in flight. A penalty, never a refusal: a slow worker still
// serves when its siblings are busier by more than its factor, and it is
// never saturated by it. The penalty follows the window: as normal samples
// replace slow ones the median falls, and it is gone once the ratio is under
// slowFactor. A worker that is passed over gathers no samples; when its last
// ones age out it is judged on nothing, loses the penalty and is tried
// again. A model with one worker has no siblings and is never judged.
//
// A gang is a worker here too, under its gang key: its siblings are the
// model's other gangs.

import (
	"sort"
	"time"
)

const (
	// slowWindow is both the window and the samples a judgement needs, so a
	// worker turns slow after slowWindow slow samples and is cleared after
	// slowWindow normal ones.
	slowWindow = 20
	// slowSampleMaxAge drops a sample from every judgement.
	slowSampleMaxAge = 2 * time.Minute
	// slowTokensFloor is the fewest tokens a TTFT is divided by.
	slowTokensFloor = 256
	// slowGapFloor is the least a gap counts as: below it the figure is how
	// the transport batched the chunks, not decode speed, and two workers
	// whose chunks arrive together would compare as noise ÷ noise.
	slowGapFloor = 2 * time.Millisecond
)

// Slow metric names, in /loadz and the worker.slow event.
const (
	SlowMetricTTFT = "ttft_per_1k_tokens"
	SlowMetricGap  = "inter_token_gap"
)

// SlowChange is a worker turning slow (Slow = true) or being cleared, for the
// leader's event stream.
type SlowChange struct {
	Model  string
	Key    string // the router key: a worker's node id, or a gang key
	Factor float64
	Metric string
	Slow   bool
}

// SlowWorker is one worker the router ranks down now.
type SlowWorker struct {
	Model   string
	Key     string
	Factor  float64
	Metric  string
	Samples int
}

type slowKey struct{ model, key string }

type slowSample struct {
	v  float64
	at time.Time
}

// slowRing is the last slowWindow samples of one metric.
type slowRing struct {
	data [slowWindow]slowSample
	n    int // written so far, capped at slowWindow
	head int
}

func (r *slowRing) push(v float64, at time.Time) {
	r.data[r.head] = slowSample{v, at}
	r.head = (r.head + 1) % slowWindow
	r.n = min(r.n+1, slowWindow)
}

// median of the samples newer than cut; ok = false with fewer than
// slowWindow of them.
func (r *slowRing) median(cut time.Time) (float64, bool) {
	var xs [slowWindow]float64
	k := 0
	for i := 0; i < r.n; i++ {
		if s := r.data[i]; s.at.After(cut) {
			xs[k] = s.v
			k++
		}
	}
	if k < slowWindow {
		return 0, false
	}
	return medianOf(xs[:k]), true
}

func (r *slowRing) fresh(cut time.Time) bool {
	for i := 0; i < r.n; i++ {
		if r.data[i].at.After(cut) {
			return true
		}
	}
	return false
}

func medianOf(xs []float64) float64 {
	sort.Float64s(xs)
	n := len(xs)
	if n%2 == 1 {
		return xs[n/2]
	}
	return (xs[n/2-1] + xs[n/2]) / 2
}

type slowWindows struct{ ttft, gap slowRing }

type slowVerdict struct {
	factor float64
	metric string
}

// slowTracker is the router's per-worker latency state. Guarded by Router.mu.
type slowTracker struct {
	factor  float64 // routing.slowFactor; 0 = off
	windows map[slowKey]*slowWindows
	slow    map[slowKey]slowVerdict // the workers ranked down now
	hook    func(SlowChange)
}

// SetSlowFactor applies routing.slowFactor. f ≤ 0 turns the judgement off:
// the windows are dropped and every worker that was slow is cleared (its
// worker.slow_cleared is sent), so no alert is left open by a policy change.
func (r *Router) SetSlowFactor(f float64) {
	if f < 0 {
		f = 0
	}
	var cleared []SlowChange
	r.mu.Lock()
	t := &r.slow
	t.factor = f
	if f == 0 {
		for k, v := range t.slow {
			cleared = append(cleared, SlowChange{Model: k.model, Key: k.key, Factor: v.factor, Metric: v.metric})
		}
		t.windows, t.slow = nil, nil
	}
	hook := t.hook
	r.mu.Unlock()
	notifySlow(hook, cleared)
}

// SlowFactor reports the applied factor (for /admin/v1 and tests).
func (r *Router) SlowFactor() float64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.slow.factor
}

// SetSlowHook is told every change of a worker's slow state, outside the
// router's lock. nil = nobody is told.
func (r *Router) SetSlowHook(fn func(SlowChange)) {
	r.mu.Lock()
	r.slow.hook = fn
	r.mu.Unlock()
}

// SlowWorkers lists the workers ranked down now, sorted by model then key.
func (r *Router) SlowWorkers() []SlowWorker {
	r.mu.RLock()
	out := make([]SlowWorker, 0, len(r.slow.slow))
	for k, v := range r.slow.slow {
		n := 0
		if w := r.slow.windows[k]; w != nil {
			n = w.ttft.n
			if v.metric == SlowMetricGap {
				n = w.gap.n
			}
		}
		out = append(out, SlowWorker{Model: k.model, Key: k.key, Factor: v.factor, Metric: v.metric, Samples: n})
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// slowPenaltyLocked is key's slow term of the rank for model. Caller holds
// r.mu.
func (r *Router) slowPenaltyLocked(model, key string) float64 {
	if r.slow.factor <= 0 {
		return 0
	}
	return r.slow.slow[slowKey{model, key}].factor
}

// noteServed records one answer key served for model: its time to first
// event, the uncached prompt tokens that time covered (tokens < 0 = no TTFT
// sample), and the mean gap between its text events (0 = none). It then
// judges the model's workers again. Off with routing.slowFactor 0.
func (r *Router) noteServed(model, key string, ttft time.Duration, tokens int, gap time.Duration, now time.Time) {
	if model == "" || key == "" {
		return
	}
	r.mu.Lock()
	t := &r.slow
	if t.factor <= 0 {
		r.mu.Unlock()
		return
	}
	if t.windows == nil {
		t.windows = make(map[slowKey]*slowWindows)
		t.slow = make(map[slowKey]slowVerdict)
	}
	k := slowKey{model, key}
	w := t.windows[k]
	if w == nil {
		w = &slowWindows{}
		t.windows[k] = w
	}
	if tokens >= 0 && ttft > 0 {
		w.ttft.push(float64(ttft)/float64(max(tokens, slowTokensFloor))*tokenWeightUnit, now)
	}
	if gap > 0 {
		w.gap.push(float64(max(gap, slowGapFloor)), now)
	}
	changes := t.judgeLocked(model, now)
	hook := t.hook
	r.mu.Unlock()
	notifySlow(hook, changes)
}

// judgeLocked recomputes every worker of model and returns what changed.
// Windows with no fresh sample left are dropped.
func (t *slowTracker) judgeLocked(model string, now time.Time) []SlowChange {
	cut := now.Add(-slowSampleMaxAge)
	type meds struct {
		ttft, gap     float64
		hasTTFT, hasG bool
	}
	all := map[string]meds{}
	for k, w := range t.windows {
		if k.model != model {
			continue
		}
		if !w.ttft.fresh(cut) && !w.gap.fresh(cut) {
			delete(t.windows, k)
			continue
		}
		var m meds
		m.ttft, m.hasTTFT = w.ttft.median(cut)
		m.gap, m.hasG = w.gap.median(cut)
		all[k.key] = m
	}
	var changes []SlowChange
	judge := func(key string, m meds) (slowVerdict, bool) {
		var sibT, sibG []float64
		for other, o := range all {
			if other == key {
				continue
			}
			if o.hasTTFT {
				sibT = append(sibT, o.ttft)
			}
			if o.hasG {
				sibG = append(sibG, o.gap)
			}
		}
		best := slowVerdict{}
		if m.hasTTFT && len(sibT) > 0 {
			if s := medianOf(sibT); s > 0 && m.ttft/s > best.factor {
				best = slowVerdict{m.ttft / s, SlowMetricTTFT}
			}
		}
		if m.hasG && len(sibG) > 0 {
			if s := medianOf(sibG); s > 0 && m.gap/s > best.factor {
				best = slowVerdict{m.gap / s, SlowMetricGap}
			}
		}
		return best, best.factor > t.factor
	}
	for key, m := range all {
		k := slowKey{model, key}
		v, slow := judge(key, m)
		was, wasSlow := t.slow[k]
		switch {
		case slow:
			t.slow[k] = v
			if !wasSlow {
				changes = append(changes, SlowChange{Model: model, Key: key, Factor: v.factor, Metric: v.metric, Slow: true})
			}
		case wasSlow:
			delete(t.slow, k)
			changes = append(changes, SlowChange{Model: model, Key: key, Factor: was.factor, Metric: was.metric})
		}
	}
	// A slow worker whose window was dropped (nothing fresh) is cleared too.
	for k, was := range t.slow {
		if _, judged := all[k.key]; k.model == model && !judged {
			delete(t.slow, k)
			changes = append(changes, SlowChange{Model: model, Key: k.key, Factor: was.factor, Metric: was.metric})
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Key < changes[j].Key })
	return changes
}

// forgetSlowLocked drops the windows and verdicts of every router key gone
// matches, for every model: a removed node (InvalidateNode) or a torn-down
// model's gangs (InvalidateModel). Caller holds r.mu. It returns the verdicts
// it cleared, for the hook.
func (t *slowTracker) forgetSlowLocked(gone func(key string) bool) []SlowChange {
	var cleared []SlowChange
	for k := range t.windows {
		if gone(k.key) {
			delete(t.windows, k)
		}
	}
	for k, v := range t.slow {
		if gone(k.key) {
			delete(t.slow, k)
			cleared = append(cleared, SlowChange{Model: k.model, Key: k.key, Factor: v.factor, Metric: v.metric})
		}
	}
	return cleared
}

func notifySlow(hook func(SlowChange), changes []SlowChange) {
	if hook == nil {
		return
	}
	for _, c := range changes {
		hook(c)
	}
}
