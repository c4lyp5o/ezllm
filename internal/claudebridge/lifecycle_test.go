package claudebridge

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Regression tests for the 2026-10-23 lifecycle audit (C2, C5).

// C2: a CLI that stops reading stdin blocks the replay write of a message
// bigger than the pipe buffer. Cancellation must end Run and reap the child;
// before, the write ignored the context.
func TestCancelUnblocksAWedgedReplayWrite(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("needs /proc to observe the child")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	bin := filepath.Join(dir, "fake-claude")
	script := "#!/bin/sh\necho $$ > " + pidFile + "\nexec sleep 600\n" // alive, never reads stdin
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Bin: bin, ConfigDir: dir}
	big, _ := json.Marshal(strings.Repeat("x", 4<<20))
	req := Request{Model: "claude-sonnet", MaxTokens: 16,
		Messages: []Message{{Role: "user", Content: big}}}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := r.Run(ctx, req, "sonnet", nil)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run returned nil from a wedged CLI")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run still blocked in the stdin write 5s after cancellation")
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("fake CLI never ran: %v", err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat("/proc/" + strconv.Itoa(pid)); err != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("child %d still alive after cancellation", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// C5: the runner timeout bounds the whole request, the wait for a slot
// included. Before, a caller with no deadline of its own queued for as long
// as the slots stayed taken.
func TestRunnerTimeoutCoversTheSlotWait(t *testing.T) {
	r := &Runner{Bin: "/nonexistent", Timeout: 100 * time.Millisecond, MaxConcurrent: 1}
	r.slots() <- struct{}{} // the only slot, held elsewhere
	defer func() { <-r.slots() }()
	req := Request{Model: "claude-sonnet", MaxTokens: 16,
		Messages: []Message{{Role: "user", Content: json.RawMessage(`"hi"`)}}}
	done := make(chan error, 1)
	go func() {
		_, err := r.Run(context.Background(), req, "sonnet", nil)
		done <- err
	}()
	select {
	case err := <-done:
		var ne *NativeError
		if !errors.As(err, &ne) || ne.Status != 504 {
			t.Fatalf("err = %v, want a 504 timeout", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("still queued 2s past a 100ms runner timeout")
	}
}
