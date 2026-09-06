//go:build linux

package agent

import (
	"os/exec"
	"syscall"
)

// applyParentDeathSignal asks the kernel to send SIGTERM to the child if
// the supervisor parent dies (e.g. SIGKILL'd, OOM-killed, crashed). The
// supervisor's `defer sup.StopAll()` only runs on a clean exit, so this
// is the last line of defence against orphaned llama-server / rpc-server
// processes holding GPU/RAM after `opod up` dies abnormally. Linux-only
// — macOS has no equivalent; orphan cleanup there relies on `opod doctor`.
func applyParentDeathSignal(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGTERM
}
