//go:build windows

package bridgeutil

import "os/exec"

// Windows has no POSIX process groups. The subscription bridges are a
// Linux-container feature (docs/*-bridge-runbook.md); these keep the release
// binary building and still kill the CLI itself on cancel.
func SetProcessGroup(cmd *exec.Cmd) {}

// KillGroup kills the CLI process itself.
func KillGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}
