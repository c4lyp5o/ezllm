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
// a CLI (or anything it spawned) running on the plan's meter. The command must
// have been started with SetProcessGroup.
func KillGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	// SetProcessGroup makes the CLI its group's leader, so the group id is
	// its pid. Looking the group up with Getpgid instead is unsafe once the
	// CLI is reaped: a recycled pid can belong to an unrelated process, even
	// one in ezllm's own group. The group id stays reserved while any member
	// lives, so killing -pid still reaches what the CLI left behind.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Process.Kill()
}
