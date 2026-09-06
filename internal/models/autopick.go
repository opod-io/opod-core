package models

import "github.com/opod-io/opod/internal/agent"

// AutoPick selects the largest catalog entry that fits the host's RAM with headroom.
// Returns the entry and ok=true on a match. headroomGB defaults to 4 if <=0.
func AutoPick(entries []Entry, caps agent.Capabilities, headroomGB int) (*Entry, bool) {
	if headroomGB <= 0 {
		headroomGB = 4
	}
	budget := caps.RAMGB - headroomGB
	if budget < 2 {
		// Tiny host — fall back to the smallest entry we have.
		if len(entries) == 0 {
			return nil, false
		}
		return &entries[0], true
	}
	// entries are size-ascending; find the largest whose min_ram_gb <= budget.
	var pick *Entry
	for i := range entries {
		e := &entries[i]
		if e.Hardware.MinRAMGB <= budget {
			pick = e
		}
	}
	if pick == nil {
		return nil, false
	}
	return pick, true
}
