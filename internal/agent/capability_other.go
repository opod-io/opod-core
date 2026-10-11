//go:build !darwin && !linux

package agent

import "github.com/opod-io/opod-sdk/nodeapi"

// detectMemAndGPU is a stub for non-Darwin, non-Linux builds (mostly for
// developer cross-compilation). Returns zero values.
func detectMemAndGPU() (int, []GPU) {
	return 0, nil
}

// detectGivenDevices: no device plugin hands this platform a subset of its
// accelerators, so there is nothing to state — nil, never an empty list.
func detectGivenDevices() *[]nodeapi.GivenDevice { return nil }
