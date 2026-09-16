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
	// Role (feature pd_roles, R9.7): "" = a complete server; "prefill" or
	// "decode" = one half of a disaggregated pair, from OPOD_WORKER_ROLE. The
	// leader routes generation to decode workers only. The KV handoff between
	// the halves is TARGET; until then a pair lives on one node.
	Role string `json:"Role,omitempty"`
	// PlanRevision is the plan revision this worker process was STARTED for
	// (feature routing_weights, R15.17), from OPOD_PLAN_REVISION. It is what
	// makes weighted routing between revisions possible: the leader groups
	// workers by it and gives each group its share. Zero = not stated, which
	// is every worker a manager has not told, and those form one group.
	PlanRevision int `json:"PlanRevision,omitempty"`
}

// GPU describes a single GPU device.
type GPU struct {
	Name   string
	VRAMGB int
}

// AcceleratorPresent reports whether this worker has a GPU to offload to.
//
// A manager states the vendor it placed the worker on (accelerator =
// OPOD_ACCELERATOR through the config contract), which covers cards whose
// tooling this binary cannot probe — the AMD and Intel builds carry no
// nvidia-smi. A worker started by hand has no such hint and falls back to what
// it detected itself.
func AcceleratorPresent(c Capabilities, accelerator string) bool {
	switch strings.ToLower(strings.TrimSpace(accelerator)) {
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
