//go:build darwin

package agent

import "os/exec"

// applyParentDeathSignal is a no-op on macOS: darwin has no equivalent
// to Linux's prctl(PR_SET_PDEATHSIG). Orphan llama-server / rpc-server
// processes after an abnormal `opod up` exit must be detected via
// `opod doctor` (listening on the configured endpoint with no opod pid)
// and reaped manually with `pkill llama-server`.
func applyParentDeathSignal(_ *exec.Cmd) {}
