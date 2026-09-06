package main

import (
	"fmt"

	"github.com/opod-io/opod/internal/agent"
	"github.com/opod-io/opod/internal/models"
)

// checkHardwareForModel compares the host's detected capabilities against the
// catalog entry's minimum requirements. Returns a non-empty message describing
// the gap when something doesn't fit; empty string means "go ahead and install."
//
// The check is intentionally conservative: it refuses on clear under-spec, not
// on borderline cases. We compare against the host's TOTAL RAM/VRAM rather
// than free RAM because:
//
//   - "free" RAM at install time is misleading (the model gets loaded later)
//   - users typically run models on machines dedicated to them
//   - false-positive refusals are worse than letting a borderline install succeed
//
// True scheduling decisions (which node should serve THIS request, given
// current load) belong in the Router, not here.
func checkHardwareForModel(entry *models.Entry) string {
	caps := agent.Detect()

	if entry.Hardware.MinRAMGB > 0 && caps.RAMGB > 0 && caps.RAMGB < entry.Hardware.MinRAMGB {
		return fmt.Sprintf(
			"model %s needs at least %d GB RAM; this machine has %d GB",
			entry.ID, entry.Hardware.MinRAMGB, caps.RAMGB,
		)
	}

	if entry.Hardware.MinVRAMGB > 0 {
		maxVRAM := 0
		for _, g := range caps.GPUs {
			if g.VRAMGB > maxVRAM {
				maxVRAM = g.VRAMGB
			}
		}
		// maxVRAM == 0 covers both "no GPU detected" and "Apple Silicon
		// unified memory" — for unified memory machines, RAM was already
		// checked above, so missing VRAM here is fine. We only refuse
		// when we detected at least one discrete GPU AND it's too small.
		if maxVRAM > 0 && maxVRAM < entry.Hardware.MinVRAMGB {
			return fmt.Sprintf(
				"model %s needs at least %d GB VRAM; largest GPU here has %d GB",
				entry.ID, entry.Hardware.MinVRAMGB, maxVRAM,
			)
		}
	}

	return ""
}
