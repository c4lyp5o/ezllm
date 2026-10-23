package codexbridge

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// LandlockHelperArg is the hidden argv[1] that turns the ezllm binary (or this
// package's test binary) into the Landlock helper: see LandlockMain.
const LandlockHelperArg = "__codex-landlock-exec"

// landlockSystemRead are the read-only roots the CLI may see; missing ones are
// skipped by the helper.
var landlockSystemRead = []string{"/usr", "/lib", "/lib32", "/lib64", "/bin", "/sbin", "/etc", "/proc", "/sys", "/dev"}

// Sandbox wraps a child in the Landlock helper. Helper is a binary that
// dispatches LandlockHelperArg to LandlockMain (the ezllm binary itself).
type Sandbox struct {
	Helper string
}

// binRoot is the CLI's own directory tree: the release root when the binary
// sits in a bin/ directory (the standalone layout keeps codex-path/ beside it),
// else its directory. The code-mode host lives next to the binary.
func binRoot(bin string) string {
	if real, err := filepath.EvalSymlinks(bin); err == nil {
		bin = real
	}
	dir := filepath.Dir(bin)
	if filepath.Base(dir) == "bin" {
		return filepath.Dir(dir)
	}
	return dir
}

// landlockLinkedFiles are /etc files the resolver and clock read that hosts
// commonly symlink out of /etc — Landlock checks the target, so Ubuntu's
// /etc/resolv.conf → /run/systemd/resolve/stub-resolv.conf left the CLI
// without DNS (measured: every request timed out).
var landlockLinkedFiles = []string{"/etc/resolv.conf", "/etc/hosts", "/etc/nsswitch.conf", "/etc/localtime", "/etc/host.conf"}

// linkedTargets returns the real paths of landlockLinkedFiles that leave the
// read-only roots.
func linkedTargets() []string {
	var out []string
	for _, f := range landlockLinkedFiles {
		real, err := filepath.EvalSymlinks(f)
		if err != nil || real == f || underAny(real, landlockSystemRead) {
			continue
		}
		out = append(out, real)
	}
	return out
}

func underAny(path string, roots []string) bool {
	for _, r := range roots {
		if path == r || strings.HasPrefix(path, r+"/") {
			return true
		}
	}
	return false
}

// wrap returns the argv that runs bin+args inside a domain where only work
// and codexHome are writable.
func (s *Sandbox) wrap(bin string, args []string, work, codexHome string) (string, []string) {
	argv := []string{LandlockHelperArg}
	for _, p := range append(append([]string{}, landlockSystemRead...), binRoot(bin)) {
		argv = append(argv, "--ro", p)
	}
	for _, f := range linkedTargets() {
		argv = append(argv, "--rofile", f)
	}
	argv = append(argv, "--rw", work, "--rw", codexHome, "--rwfile", os.DevNull, "--", bin)
	return s.Helper, append(argv, args...)
}

// ProbeLandlock proves enforcement on this host, not just availability: the
// helper restricts itself to one temp dir, must be able to write there, and
// must be refused a read of a file outside it. Run through the real helper
// binary, so a container seccomp profile that blocks the syscalls shows here.
func ProbeLandlock(helper string) (abi int, err error) {
	abi, err = LandlockABI()
	if err != nil {
		return 0, err
	}
	allowed, err := os.MkdirTemp("", "codex-bridge-landlock-")
	if err != nil {
		return abi, err
	}
	defer os.RemoveAll(allowed)
	outside, err := os.CreateTemp("", "codex-bridge-landlock-forbidden-")
	if err != nil {
		return abi, err
	}
	outside.Close()
	defer os.Remove(outside.Name())
	out, err := exec.Command(helper, LandlockHelperArg, "--selftest", allowed, outside.Name()).CombinedOutput()
	if err != nil {
		return abi, fmt.Errorf("the Landlock self-test failed: %s (%v)", strings.TrimSpace(string(out)), err)
	}
	return abi, nil
}

// CodeModeHost is the code-mode runtime the CLI execs for client tools; it
// must sit next to the binary (measured: the standalone release ships it as a
// sibling, codex-code-mode-host).
func CodeModeHost(bin string) string {
	if real, err := filepath.EvalSymlinks(bin); err == nil {
		bin = real
	}
	return filepath.Join(filepath.Dir(bin), "codex-code-mode-host")
}
