package main

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/leader"
)

// TestVersionFeaturesIsTheContract: `opod version --features` is what the image
// build stamps on the leader image as io.opod.leader.features, and a manager
// decides from that label whether a pinned leader has a TLS listener. The line
// must be the leader's advertised feature keys, in order, with nothing added —
// a drift here would let a manager render TLS for a leader without it.
func TestVersionFeaturesIsTheContract(t *testing.T) {
	var buf bytes.Buffer
	printFeatures(&buf)
	line := buf.String()
	if !strings.HasSuffix(line, "\n") || strings.Count(line, "\n") != 1 {
		t.Fatalf("one line, newline-terminated: %q", line)
	}
	got := strings.Split(strings.TrimSpace(line), ",")
	if want := leader.FeatureKeys(); !slices.Equal(got, want) {
		t.Fatalf("version --features = %v, the leader advertises %v", got, want)
	}
	if !slices.IsSorted(got) {
		t.Errorf("not sorted: %v", got)
	}
	// The key a manager reads before it renders a leader for TLS.
	if !slices.Contains(got, "tls_listener") {
		t.Errorf("tls_listener missing from %v", got)
	}
	if strings.ContainsAny(line, " \t") {
		t.Errorf("a label value with whitespace is split by shells and build lanes: %q", line)
	}
}
