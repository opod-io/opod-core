//go:build !unix

package agent

import (
	"fmt"
	"os/exec"
	"syscall"
)

// applyProcessGroup is a no-op on non-unix platforms: there is no POSIX
// process group to create. Stop() falls back to signaling the process
// itself (cmd.Process.Signal / Kill).
func applyProcessGroup(_ *exec.Cmd) {}

// applyParentDeathSignal is a no-op on non-unix platforms: there is no
// equivalent to Linux's prctl(PR_SET_PDEATHSIG).
func applyParentDeathSignal(_ *exec.Cmd) {}

// signalGroup always errors on non-unix platforms so callers take their
// per-process fallback path (cmd.Process.Signal / Kill).
func signalGroup(pid int, sig syscall.Signal) error {
	return fmt.Errorf("process group signaling not supported on this platform (pid %d, sig %v)", pid, sig)
}
