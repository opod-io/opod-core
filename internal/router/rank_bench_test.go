package router

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// BenchmarkRankEightWorkers is the per-pick cost of the shared ranking over
// eight workers with a pulled load sample each, the path every request takes
// (T28.10, T28.12: the token ledger and the slow penalty are read here).
func BenchmarkRankEightWorkers(b *testing.B) {
	r := New(nil, nil)
	sampled := time.Now()
	r.SetLoadSource(func(string) (LoadSignal, bool) {
		return LoadSignal{QueueDepth: 1, KVUsedPct: 40, SampledAt: sampled}, true
	})
	r.SetLoadAware(1, 95, false)
	cands := make([]candidate, 8)
	for i := range cands {
		cands[i] = workerCandidate(fmt.Sprintf("w%d", i), false)
		r.incInflight(cands[i].key, "m")
	}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.rankCandidates(ctx, "m", cands, nil)
	}
}
