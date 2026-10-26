package engines

// An engine's own latency histograms (PLAN T24.25, feature "engine_ttft").
//
// vLLM and SGLang publish time to first token as a Prometheus histogram:
// cumulative bucket counts since the engine started. A median over the
// engine's whole life says nothing about the last minute, and the leader's
// own ttft_p50_ms is a one-minute window — so a driver keeps a few snapshots
// of the buckets (it persists between samples, as its RateTracker does) and
// takes the quantile of the observations between the snapshot about a
// window ago and now.

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// HistBucket is one cumulative bucket: the observations ≤ LE.
type HistBucket struct {
	LE    float64
	Count float64
}

// PromHistogram returns the cumulative buckets of one histogram in Prometheus
// text exposition, ascending by upper bound, summed over its series (one per
// model name, or per engine of a data-parallel server). nil when the engine
// publishes no such histogram. Only that histogram's _bucket lines go to the
// parser, so the rest of the exposition costs nothing and cannot break it.
func PromHistogram(text, name string) []HistBucket {
	series := name + "_bucket"
	var b strings.Builder
	for _, line := range strings.Split(text, "\n") {
		if t := strings.TrimSpace(line); strings.HasPrefix(t, series+"{") {
			b.WriteString(t + "\n")
		}
	}
	if b.Len() == 0 {
		return nil
	}
	parser := expfmt.NewTextParser(model.LegacyValidation)
	fams, err := parser.TextToMetricFamilies(strings.NewReader(b.String()))
	if err != nil || fams[series] == nil {
		return nil
	}
	byLE := map[float64]float64{}
	for _, m := range fams[series].GetMetric() {
		le := math.NaN()
		for _, l := range m.GetLabel() {
			if l.GetName() == "le" {
				if v, err := strconv.ParseFloat(l.GetValue(), 64); err == nil {
					le = v
				}
			}
		}
		if math.IsNaN(le) {
			continue
		}
		byLE[le] += m.GetUntyped().GetValue()
	}
	if len(byLE) == 0 {
		return nil
	}
	out := make([]HistBucket, 0, len(byLE))
	for le, n := range byLE {
		out = append(out, HistBucket{LE: le, Count: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LE < out[j].LE })
	return out
}

// histogramQuantile is Prometheus's histogram_quantile over cumulative
// buckets: the bucket the q-th observation falls in, linearly interpolated
// inside it (from 0 for the first bucket). A rank in the +Inf bucket answers
// the largest finite bound. ok = false with no observations.
func histogramQuantile(q float64, b []HistBucket) (float64, bool) {
	if len(b) == 0 {
		return 0, false
	}
	total := b[len(b)-1].Count
	if total <= 0 {
		return 0, false
	}
	rank := q * total
	lower, below := 0.0, 0.0
	for i, bk := range b {
		if bk.Count >= rank {
			if math.IsInf(bk.LE, 1) {
				if i == 0 {
					return 0, false
				}
				return b[i-1].LE, true
			}
			in := bk.Count - below
			if in <= 0 {
				return bk.LE, true
			}
			return lower + (bk.LE-lower)*(rank-below)/in, true
		}
		lower, below = bk.LE, bk.Count
	}
	return lower, true
}

// histSnap is one reading of a cumulative histogram.
type histSnap struct {
	at time.Time
	b  []HistBucket
}

// maxHistSnaps bounds the snapshots a window keeps whatever the sample rate.
const maxHistSnaps = 256

// HistogramWindow turns a cumulative histogram into a quantile over about the
// last window of observations. The zero value is ready to use.
type HistogramWindow struct {
	mu    sync.Mutex
	snaps []histSnap
}

// Quantile records the histogram read at now and returns the q-quantile of
// the observations since the newest earlier reading at least window old (or
// the oldest reading, until one is that old). ok = false on the first
// reading, after a reset (a bucket went down: the engine restarted), when the
// buckets changed shape, or when nothing was observed in between — "not
// said", never a zero.
func (w *HistogramWindow) Quantile(now time.Time, cur []HistBucket, q float64, window time.Duration) (float64, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(cur) == 0 {
		w.snaps = nil
		return 0, false
	}
	base := -1
	for i, s := range w.snaps {
		if !s.at.After(now.Add(-window)) {
			base = i // the newest reading at least a window old
		}
	}
	if base < 0 && len(w.snaps) > 0 {
		base = 0
	}
	var prev []HistBucket
	if base >= 0 {
		prev = w.snaps[base].b
		w.snaps = w.snaps[base:]
	}
	w.snaps = append(w.snaps, histSnap{at: now, b: cur})
	if len(w.snaps) > maxHistSnaps {
		w.snaps = w.snaps[len(w.snaps)-maxHistSnaps:]
	}
	if prev == nil || len(prev) != len(cur) {
		if prev != nil {
			w.snaps = w.snaps[len(w.snaps)-1:]
		}
		return 0, false
	}
	delta := make([]HistBucket, len(cur))
	for i := range cur {
		if cur[i].LE != prev[i].LE || cur[i].Count < prev[i].Count {
			w.snaps = w.snaps[len(w.snaps)-1:] // reset or reshaped: start again from now
			return 0, false
		}
		delta[i] = HistBucket{LE: cur[i].LE, Count: cur[i].Count - prev[i].Count}
	}
	return histogramQuantile(q, delta)
}

// ttftWindow is the window an engine's TTFT median covers: the leader's own
// ttft_p50_ms is a one-minute window, and "leader + network" is the
// difference of the two.
const ttftWindow = time.Minute

// TTFTP50Ms reads histogram name (seconds) from an engine's exposition and
// returns its median over about the last minute in milliseconds, or nil when
// the engine does not publish it or served nothing in the window.
func (w *HistogramWindow) TTFTP50Ms(text, name string, now time.Time) *int64 {
	sec, ok := w.Quantile(now, PromHistogram(text, name), 0.5, ttftWindow)
	if !ok {
		return nil
	}
	return new(int64(math.Round(sec * 1000)))
}
