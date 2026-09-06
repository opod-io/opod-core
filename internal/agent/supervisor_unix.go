//go:build unix

package agent

import (
	"os/exec"
	"syscall"
)

// applyProcessGroup puts a launched process in its own process group so
// the supervisor can later signal the whole group (parent + any children
// the process itself forked). Used during graceful shutdown so a single
// `kill -TERM <pid>` cascades to grandchildren — important for engines
// like llama-server that fork worker threads or download helpers.
func applyProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// signalGroup sends sig to the process group led by pid. A negative pid
// to syscall.Kill targets the whole group. Returns the error from the
// syscall so callers can fall back to a per-pid signal if the group
// signal fails (e.g. process group already gone).
func signalGroup(pid int, sig syscall.Signal) error {
	return syscall.Kill(-pid, sig)
}
