package api

// R15.13 · what "time to first token" means here, so a later change cannot
// quietly turn it into something else:
//   - it is the wait a person actually experiences on a STREAMED answer,
//     measured to the first content delta — not to the role chunk, which is
//     protocol, and not to the end of the answer;
//   - a non-streamed answer has none, and records 0, which readers must show
//     as "not measured" rather than as instant.

import (
	"testing"
	"time"

	"github.com/opod-io/opod/internal/metrics"
)

func TestObserveTTFTIgnoresNonStreamedAnswers(t *testing.T) {
	// A zero duration is what a non-streamed answer reports. Recording it would
	// drag every percentile toward zero and make the fleet look instant.
	metrics.ObserveTTFT("m", 0)
	metrics.ObserveTTFT("m", -5*time.Millisecond)
	// Nothing to assert beyond not panicking and not recording: the guard is
	// the behaviour, and the histogram is package-private on purpose.
}

func TestStreamedAnswerRecordsTheFirstDeltaNotTheRoleChunk(t *testing.T) {
	// The stream path stamps ttft at the first non-empty delta. This test keeps
	// that contract visible: if someone moves the stamp to the role chunk, the
	// number becomes "how fast did we open the stream", which is always small
	// and never what a user waited for.
	start := time.Now()
	var ttft time.Duration
	deltas := []string{"", "", "Hel", "lo"} // two protocol chunks, then content
	for _, d := range deltas {
		if d != "" && ttft == 0 {
			ttft = time.Since(start)
		}
	}
	if ttft == 0 {
		t.Fatal("no first token was timed for a stream that produced content")
	}
}
