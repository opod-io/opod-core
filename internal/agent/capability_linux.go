//go:build linux

package agent

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// detectMemAndGPU returns (ramGB, gpus). NVIDIA GPUs are detected via
// `nvidia-smi` when available; otherwise the GPU list is empty.
func detectMemAndGPU() (int, []GPU) {
	ramGB := 0
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "MemTotal:") {
				parts := strings.Fields(line)
				if len(parts) >= 2 {
					if kb, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
						ramGB = int(kb / (1024 * 1024))
					}
				}
				break
			}
		}
	}
	var gpus []GPU
	if out, err := exec.Command("nvidia-smi",
		"--query-gpu=name,memory.total", "--format=csv,noheader,nounits").Output(); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			parts := strings.Split(line, ", ")
			if len(parts) < 2 {
				continue
			}
			vramMiB, _ := strconv.Atoi(strings.TrimSpace(parts[1]))
			gpus = append(gpus, GPU{
				Name:   strings.TrimSpace(parts[0]),
				VRAMGB: vramMiB / 1024,
			})
		}
	}
	return ramGB, gpus
}
