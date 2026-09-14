package scheduler

import (
	"strings"
	"testing"
)

// Build item 5: TP × PP is checked against the gang's GPUs — parts × devices
// per part — and the devices of one part are always inside one tensor group.
func TestParallelismResolveWithDevicesPerRank(t *testing.T) {
	cases := []struct {
		in           Parallelism
		ranks        int
		tp, pp       int
		wantErr      string
		devicesOfOut int
	}{
		{Parallelism{}, 2, 1, 2, "", 1},                                          // default: one GPU per part → TP1 × PP2
		{Parallelism{DevicesPerRank: 4}, 2, 4, 2, "", 4},                         // 2 parts × 4 GPUs → TP4 inside each, PP2 across
		{Parallelism{TP: 8, DevicesPerRank: 4}, 2, 8, 1, "", 4},                  // TP may span parts when asked (measured, not assumed)
		{Parallelism{PP: 2, DevicesPerRank: 4}, 2, 4, 2, "", 4},                  // PP alone → TP fills the rest
		{Parallelism{TP: 2, DevicesPerRank: 4}, 2, 0, 0, "multiple of the 4", 0}, // a tensor group cannot split a part
		{Parallelism{TP: 3, DevicesPerRank: 1}, 4, 0, 0, "does not divide", 0},
		{Parallelism{TP: 2, PP: 4}, 2, 0, 0, "gang has 2 GPUs", 0},
	}
	for _, c := range cases {
		got, err := c.in.resolve(c.ranks)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("%+v ranks=%d: err %v, want %q", c.in, c.ranks, err, c.wantErr)
			}
			continue
		}
		if err != nil || got.TP != c.tp || got.PP != c.pp || got.DevicesPerRank != c.devicesOfOut {
			t.Errorf("%+v ranks=%d: got %+v (%v), want tp=%d pp=%d k=%d", c.in, c.ranks, got, err, c.tp, c.pp, c.devicesOfOut)
		}
	}
}
