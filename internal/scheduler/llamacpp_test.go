package scheduler

import (
	"context"
	"testing"
	"time"
)

// The auto-spawned llama-server's readiness bound is the caller's budget: an
// inner default shorter than the caller's deadline is what left a first
// `opod up` failing while llama-server was still downloading its model.
func TestAutoSpawnReadyTimeout(t *testing.T) {
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	long, cancelLong := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancelLong()
	short, cancelShort := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelShort()

	cases := []struct {
		name     string
		ctx      context.Context
		min, max time.Duration
	}{
		{"no deadline takes the default", context.Background(), autoSpawnReadyDefault, autoSpawnReadyDefault},
		{"a long deadline is used whole, past the old 30 s", long, 29 * time.Minute, 30 * time.Minute},
		{"a short deadline wins over the default", short, time.Millisecond, 2 * time.Second},
		{"an expired deadline fails at once", expired, time.Millisecond, time.Millisecond},
	}
	for _, c := range cases {
		got := autoSpawnReadyTimeout(c.ctx)
		if got < c.min || got > c.max {
			t.Errorf("%s: got %s, want between %s and %s", c.name, got, c.min, c.max)
		}
	}
}
