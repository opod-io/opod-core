// Package agent contains node-local logic: hardware capability detection,
// the heartbeat loop, the worker HTTP server, and the process supervisor
// used for sharded-model orchestration.
package agent

import (
	"os"
	"runtime"
	"strings"
)

// Capabilities summarizes the host machine's resources.
type Capabilities struct {
	Hostname string
	OS       string
	Arch     string
	CPUCores int
	RAMGB    int
	GPUs     []GPU
}

// GPU describes a single GPU device.
type GPU struct {
	Name   string
	VRAMGB int
}

// AcceleratorPresent reports whether this worker has a GPU to offload to.
//
// A control plane states the vendor it placed the worker on, which covers cards
// whose tooling this binary cannot probe — the AMD and Intel builds carry no
// nvidia-smi. A worker started by hand has no such hint and falls back to what
// it detected itself.
func AcceleratorPresent(c Capabilities) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("OPOD_ACCELERATOR"))) {
	case "":
		return len(c.GPUs) > 0 // no control plane, or one older than this field
	case "none":
		return false
	default:
		return true
	}
}

// Detect returns the host's Capabilities. Platform-specific details are
// filled in by capability_<goos>.go via detectMemAndGPU.
func Detect() Capabilities {
	hostname, _ := os.Hostname()
	c := Capabilities{
		Hostname: hostname,
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
		CPUCores: runtime.NumCPU(),
	}
	c.RAMGB, c.GPUs = detectMemAndGPU()
	return c
}
