package router

import (
	"fmt"
	"testing"

	"github.com/opod-io/opod/internal/store"
)

// After the first sight of a node's blob the pick path parses nothing: the
// role and revision come from the memo, and a changed blob (a re-registration
// with a new role) is parsed once more (PLAN T15.6).
func TestCapsAreParsedOncePerBlob(t *testing.T) {
	r := New(nil, nil)
	nodes := make([]*store.Node, 8)
	for i := range nodes {
		nodes[i] = &store.Node{ID: fmt.Sprintf("n%d", i), HardwareJSON: fmt.Sprintf(`{"Role":"decode","PlanRevision":%d,"GPUs":[{"Name":"pad","VRAMMiB":49140}]}`, i%2+1)}
	}
	for _, n := range nodes {
		if c := r.capsOf(n); c.role != "decode" || c.revision == 0 {
			t.Fatalf("first parse: %+v", c)
		}
	}
	allocs := testing.AllocsPerRun(100, func() {
		for _, n := range nodes {
			_ = r.capsOf(n)
		}
	})
	if allocs != 0 {
		t.Fatalf("a warm capsOf over 8 nodes allocated %.0f times: the blob is being parsed again", allocs)
	}
	nodes[0].HardwareJSON = `{"Role":"prefill","PlanRevision":7}`
	if c := r.capsOf(nodes[0]); c.role != "prefill" || c.revision != 7 {
		t.Fatalf("a changed blob is re-read: %+v", c)
	}
	r.InvalidateNode("n0")
	if r.HoldsNode("n0") {
		t.Fatal("InvalidateNode leaves the memo")
	}
}

func BenchmarkCapsOfWarm(b *testing.B) {
	r := New(nil, nil)
	nodes := make([]*store.Node, 8)
	for i := range nodes {
		nodes[i] = &store.Node{ID: fmt.Sprintf("n%d", i), HardwareJSON: `{"Role":"decode","PlanRevision":1,"GPUs":[{"Name":"pad","VRAMMiB":49140}]}`}
	}
	for _, n := range nodes {
		r.capsOf(n)
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		for _, n := range nodes {
			_ = r.capsOf(n)
		}
	}
}
