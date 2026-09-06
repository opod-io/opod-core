//go:build !darwin && !linux

package agent

// detectMemAndGPU is a stub for non-Darwin, non-Linux builds (mostly for
// developer cross-compilation). Returns zero values.
func detectMemAndGPU() (int, []GPU) {
	return 0, nil
}
