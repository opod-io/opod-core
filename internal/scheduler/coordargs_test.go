package scheduler

import (
	"slices"
	"strings"
	"testing"
)

// T11.3, found on a cluster: the leader scrapes a gang's coordinator for the
// pressure the gang's rpc-server parts cannot report — and the coordinator it
// launches had no /metrics to scrape.
//
// llama-server exposes /metrics only with --metrics. The worker passes it for
// the engine IT launches (agent/server.go), the coordinator was missed, and
// the coordinator is the one that matters for a gang: it holds the KV cache
// and it queues. On kind a serving two-part gang read `workers 2, reporting 0,
// kv_used_pct 0` — the count was right and the pressure was still a constant,
// which looks exactly like the defect the scrape was written to fix.
func TestCoordinatorIsLaunchedWithMetrics(t *testing.T) {
	args := coordinatorArgs("/data/models/m.gguf", 9001, "127.0.0.1", []string{"10.0.0.2:50052"})
	if !slices.Contains(args, "--metrics") {
		t.Fatalf("a gang's coordinator must expose /metrics or its pressure is unreadable: %v", args)
	}

	// The rest of the line, so the flag cannot be added by breaking it.
	for _, want := range [][2]string{{"-m", "/data/models/m.gguf"}, {"--port", "9001"}, {"--host", "127.0.0.1"}} {
		i := slices.Index(args, want[0])
		if i < 0 || i+1 >= len(args) || args[i+1] != want[1] {
			t.Errorf("%s %s missing from %v", want[0], want[1], args)
		}
	}
	if i := slices.Index(args, "--rpc"); i < 0 || args[i+1] != "10.0.0.2:50052" {
		t.Errorf("the rpc backends must be passed: %v", args)
	}

	// One shard is a plain whole-model llama-server: no --rpc at all, because
	// an empty list would make it abort on a backend that does not exist.
	solo := coordinatorArgs("/data/models/m.gguf", 9001, "0.0.0.0", nil)
	if slices.Contains(solo, "--rpc") {
		t.Errorf("a 1-shard coordinator takes no --rpc: %v", solo)
	}
	if !slices.Contains(solo, "--metrics") {
		t.Errorf("a 1-shard coordinator reports pressure too: %v", solo)
	}
	if strings.Join(solo, " ") == strings.Join(args, " ") {
		t.Error("the two shapes must differ")
	}
}
