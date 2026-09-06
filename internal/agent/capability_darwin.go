//go:build darwin

package agent

import (
	"os/exec"
	"strconv"
	"strings"
)

// detectMemAndGPU returns (ramGB, gpus). On Apple Silicon the GPU shares
// system memory, so we report unified memory as a single GPU whose VRAM equals
// total RAM. Errors are swallowed — best-effort detection.
func detectMemAndGPU() (int, []GPU) {
	ramGB := 0
	if out, err := exec.Command("sysctl", "-n", "hw.memsize").Output(); err == nil {
		if n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); err == nil {
			ramGB = int(n / (1024 * 1024 * 1024))
		}
	}
	chip := "Apple Silicon"
	if out, err := exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output(); err == nil {
		chip = strings.TrimSpace(string(out))
	}
	gpus := []GPU{{Name: chip + " (unified memory)", VRAMGB: ramGB}}
	return ramGB, gpus
}
