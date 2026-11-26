//go:build unix

package copilotbridge

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"
)

// alive reports whether pid still runs (a zombie counts as gone).
func alive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	f := strings.Fields(string(b))
	return len(f) < 3 || f[2] != "Z"
}

func grandchildPID(t *testing.T, pidFile string) int {
	t.Helper()
	var pid int
	for i := 0; i < 100; i++ {
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, err = strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("stub never wrote the grandchild pid")
	return 0
}

// A cancelled -p turn kills what the CLI started, not only the CLI.
func TestRunKillsTheCLIsChildren(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	bin := stubCLI(t, "sleep 600 >/dev/null 2>&1 &\necho $! > "+pidFile+"\nexec sleep 600")
	r := &Runner{Bin: bin, Home: t.TempDir(), Timeout: 300 * time.Millisecond}
	_, _ = r.Run(context.Background(), "m", "p", nil)
	pid := grandchildPID(t, pidFile)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	deadline := time.Now().Add(2 * time.Second)
	for alive(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if alive(pid) {
		t.Fatal("the CLI's child outlived the cancelled turn")
	}
}

func TestClipDetailKeepsUTF8(t *testing.T) {
	s := clipDetail(strings.Repeat("a", maxErrorDetail-1) + "é" + "tail")
	if !utf8.ValidString(s) || !strings.HasSuffix(s, "...") {
		t.Fatalf("clipDetail split a rune: %q", s)
	}
}
