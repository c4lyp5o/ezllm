//go:build unix

package codexbridge

import (
	"os/exec"
	"syscall"
)

// setProcessGroup starts the CLI in its own process group so killGroup can
// reach everything it spawns.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killGroup kills the whole process group: a cancelled request must not leave a
// CLI (or anything it spawned) running on the plan's meter.
func killGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
	_ = cmd.Process.Kill()
}
