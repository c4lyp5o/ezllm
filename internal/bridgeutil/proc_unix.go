//go:build unix

package bridgeutil

import (
	"os/exec"
	"syscall"
)

// SetProcessGroup starts a bridge CLI in its own process group so KillGroup
// can reach everything it spawns.
func SetProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// KillGroup kills the whole process group: a cancelled request must not leave
// a CLI (or anything it spawned) running on the plan's meter.
func KillGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
	_ = cmd.Process.Kill()
}
