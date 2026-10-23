//go:build windows

package codexbridge

import "os/exec"

// Windows has no POSIX process groups. The bridge is a Linux-container feature
// (docs/codex-bridge-runbook.md); these keep the release binary building and
// still kill the CLI itself on cancel.
func setProcessGroup(cmd *exec.Cmd) {}

func killGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}
