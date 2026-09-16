package router

// R15.17 · weighted routing between plan revisions. The properties that make a
// 10 % canary a 10 % canary, and the one that stops it becoming an outage.

import (
	"testing"
)

// roller replays a fixed sequence, so a distribution test is exact rather than
// flaky: chooseRevision is given every value in [0,total) once.
func sweep(ws []RevisionWeight, live map[int]int, total int) map[int]int {
	got := map[int]int{}
	for i := 0; i < total; i++ {
		n := i
		got[chooseRevision(ws, live, func(int) int { return n })]++
	}
	return got
}

func TestWeightsSplitTrafficInProportion(t *testing.T) {
	ws := []RevisionWeight{{Revision: 1, Weight: 9}, {Revision: 2, Weight: 1}}
	got := sweep(ws, map[int]int{1: 3, 2: 1}, 10)
	if got[1] != 9 || got[2] != 1 {
		t.Fatalf("9:1 split produced %v", got)
	}
}

// A weight is a share, not a percentage: 90/10 and 9/1 are the same split, and
// 45/5 is too. A manager cannot create a hole by not adding up to 100.
func TestWeightsAreSharesNotPercentages(t *testing.T) {
	live := map[int]int{1: 1, 2: 1}
	a := sweep([]RevisionWeight{{1, 9}, {2, 1}}, live, 10)
	b := sweep([]RevisionWeight{{1, 45}, {2, 5}}, live, 50)
	if float64(a[2])/10 != float64(b[2])/50 {
		t.Fatalf("the same share produced different splits: %v vs %v", a, b)
	}
}

// THE important one. A canary whose only pod is restarting must not swallow
// its share of the traffic: a revision with no live worker is skipped and the
// rest take it.
func TestARevisionWithNoWorkerGetsNoTraffic(t *testing.T) {
	ws := []RevisionWeight{{Revision: 1, Weight: 9}, {Revision: 2, Weight: 1}}
	got := sweep(ws, map[int]int{1: 2}, 10) // revision 2 has nothing live
	if got[2] != 0 {
		t.Fatalf("traffic was sent to a revision with no worker: %v", got)
	}
	// One candidate left means nothing to choose between: the caller routes
	// over every worker exactly as it did before weights existed.
	if got[0] != 10 {
		t.Fatalf("with one live revision the split should stand down, got %v", got)
	}
}

func TestNoWeightsMeansNoSplit(t *testing.T) {
	if got := sweep(nil, map[int]int{1: 1, 2: 1}, 4); got[0] != 4 {
		t.Fatalf("an unconfigured split still chose a group: %v", got)
	}
}

func TestZeroAndNegativeWeightsAreDropped(t *testing.T) {
	r := &Router{}
	r.SetRevisionWeights([]RevisionWeight{{Revision: 1, Weight: 5}, {Revision: 2, Weight: 0}, {Revision: 0, Weight: 7}, {Revision: 3, Weight: -1}})
	got := r.revisionWeights()
	if len(got) != 1 || got[0].Revision != 1 {
		t.Fatalf("only a positive revision with a positive weight is a group: %+v", got)
	}
}
