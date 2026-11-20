//go:build unix

package bridgeutil

import (
	"bufio"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// gone reports whether pid has exited (absent, or a zombie awaiting reaping).
func gone(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return true
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return os.IsNotExist(err)
	}
	f := strings.Fields(string(b)[strings.LastIndexByte(string(b), ')')+1:])
	return len(f) > 0 && f[0] == "Z"
}

func TestKillGroupReachesGrandchild(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 30 & echo $!; wait")
	SetProcessGroup(cmd)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	child, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })

	KillGroup(cmd)
	_ = cmd.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for !gone(child) {
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d survived KillGroup", child)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestKillGroupNilSafe(t *testing.T) {
	KillGroup(nil)
	KillGroup(exec.Command("true")) // never started
}
