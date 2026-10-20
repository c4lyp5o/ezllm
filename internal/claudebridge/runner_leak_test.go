package claudebridge

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// H4: an early return from Run (here: the client's sink failing on the
// first frame) used to leave the stdout reader blocked forever on an
// unbuffered send, and the killed CLI unreaped as a zombie. Both are
// per-failed-request leaks on a long-running bridge.
func TestRunEarlyReturnReapsChildAndReader(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("needs /proc to observe the child's state")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	// A fake CLI that records its pid, then streams frames forever: the
	// reader always has a next line to send.
	script := "#!/bin/sh\necho $$ > " + pidFile + "\nwhile :; do echo '{\"type\":\"stream_event\",\"event\":{}}'; done\n"
	bin := filepath.Join(dir, "fake-claude")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	before := runtime.NumGoroutine()
	r := &Runner{Bin: bin, ConfigDir: dir, Timeout: 10 * time.Second}
	req := Request{Model: "claude-sonnet", MaxTokens: 16,
		Messages: []Message{{Role: "user", Content: json.RawMessage(`"hi"`)}}}
	sinkErr := errors.New("client went away")
	_, err := r.Run(context.Background(), req, "sonnet", func(Frame) error { return sinkErr })
	if !errors.Is(err, sinkErr) {
		t.Fatalf("Run = %v, want the sink error", err)
	}

	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("fake CLI never ran: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	// Reaped means the pid is gone; a killed-but-unwaited child stays "Z".
	deadline := time.Now().Add(3 * time.Second)
	for {
		stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			break // gone: reaped
		}
		if time.Now().After(deadline) {
			t.Fatalf("child %d not reaped after Run returned: %s", pid, stat)
		}
		time.Sleep(20 * time.Millisecond)
	}
	for {
		if runtime.NumGoroutine() <= before {
			break
		}
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			n := runtime.Stack(buf, true)
			t.Fatalf("goroutines leaked: before=%d now=%d\n%s", before, runtime.NumGoroutine(), buf[:n])
		}
		time.Sleep(20 * time.Millisecond)
	}
}
