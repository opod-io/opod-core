package router

import (
	"sort"
	"time"
)

// Per-request sort modes (mirrors internal/models constants; duplicated
// here to avoid an import the router otherwise doesn't need).
const (
	SortPrice      = "price"
	SortLatency    = "latency"
	SortThroughput = "throughput"
)

// SetPriceResolver wires a per-model $/1K-token lookup into the router
// for `sort: price` / `:floor`. The resolver should return the combined
// prompt+completion rate; 0 means free (local open-weight models) and
// sorts first. Typically a closure over models.PriceFor + the catalog.
func (r *Router) SetPriceResolver(f func(modelID string) float64) {
	r.priceFn = f
}

// sortChain stably reorders the candidate chain by the requested
// metric. The primary loses its special position on purpose — the
// caller asked for "cheapest first" / "fastest first", and a stable
// sort preserves the catalog's preference order among ties.
//
// Unknown metrics return the chain unchanged (Clamp normally rejects
// them earlier).
func (r *Router) sortChain(chain []string, mode string) []string {
	if len(chain) < 2 {
		return chain
	}
	out := make([]string, len(chain))
	copy(out, chain)
	switch mode {
	case SortPrice:
		if r.priceFn == nil {
			return chain
		}
		sort.SliceStable(out, func(i, j int) bool {
			return r.priceFn(out[i]) < r.priceFn(out[j])
		})
	case SortLatency:
		// Models with no samples sort last (unknown ≠ fast); ties keep
		// catalog order.
		key := func(m string) time.Duration {
			if p := r.latency.p95(m); p > 0 {
				return p
			}
			return time.Duration(1<<63 - 1)
		}
		sort.SliceStable(out, func(i, j int) bool { return key(out[i]) < key(out[j]) })
	case SortThroughput:
		// Higher tokens/sec first; models with no samples (0) sort last.
		sort.SliceStable(out, func(i, j int) bool {
			return r.latency.throughput(out[i]) > r.latency.throughput(out[j])
		})
	default:
		return chain
	}
	return out
}
