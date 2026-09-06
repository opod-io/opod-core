// Package agent contains node-local logic: hardware capability detection,
// the heartbeat loop, the worker HTTP server, and the process supervisor
// used for sharded-model orchestration.
package agent

import (
	"os"
	"runtime"
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
