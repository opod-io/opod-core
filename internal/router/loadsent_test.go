package router

import (
	"testing"
	"time"

	"github.com/opod-io/opod-core/internal/store"
)

// ADR-109: requests sent after a worker's sample count toward its rank, so a
// burst between two pulls spreads instead of herding on one worker.
func TestRequestsSentAfterTheSampleCountTowardTheRank(t *testing.T) {
	r := New(nil, nil)
	sampled := time.Now()
	r.SetLoadSource(func(string) (LoadSignal, bool) { return LoadSignal{QueueDepth: 1, SampledAt: sampled}, true })
	r.mu.Lock()
	r.noteSentLocked("w", sampled.Add(-time.Second)) // before the sample: already in its queue figure
	r.noteSentLocked("w", sampled.Add(10*time.Millisecond))
	r.noteSentLocked("w", sampled.Add(20*time.Millisecond))
	r.mu.Unlock()
	r.mu.RLock()
	_, score := r.loadRank("w")
	r.mu.RUnlock()
	if score != 3 { // queue 1 + 2 sent since; in-flight 0 here
		t.Fatalf("score %v, want queue 1 + 2 sent after the sample", score)
	}
}

func TestTheSentRecordIsBounded(t *testing.T) {
	r := New(nil, nil)
	now := time.Now()
	r.mu.Lock()
	for i := 0; i < 200; i++ {
		r.noteSentLocked("w", now)
	}
	r.noteSentLocked("x", now.Add(-time.Minute))
	r.noteSentLocked("x", now)
	n, m := len(r.sent["w"]), len(r.sent["x"])
	r.mu.Unlock()
	if n != sentKeep || m != 1 {
		t.Fatalf("kept %d and %d, want %d and 1", n, m, sentKeep)
	}
}

// A gang ranks on its head node's pulled sample, and what was sent to the gang
// after that sample counts too.
func TestAGangCountsWhatWasSentAfterItsHeadsSample(t *testing.T) {
	r := New(nil, nil)
	sampled := time.Now()
	r.SetLoadSource(func(node string) (LoadSignal, bool) {
		if node != "head" {
			return LoadSignal{}, false
		}
		return LoadSignal{QueueDepth: 2, SampledAt: sampled}, true
	})
	r.mu.Lock()
	r.noteSentLocked(gangKey("m", "g0"), sampled.Add(5*time.Millisecond))
	r.mu.Unlock()
	r.mu.RLock()
	_, score := r.gangRank("g0", "m", store.Shard{NodeID: "head"})
	r.mu.RUnlock()
	if score != 3 {
		t.Fatalf("gang score %v, want queue 2 + 1 sent after the sample", score)
	}
}
