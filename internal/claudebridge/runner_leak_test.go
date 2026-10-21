package claudebridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// Measured on CLI 2.1.306: after a tool call native answers the unknown tool
// itself, then --max-turns 1 ends the run with is_error:true,
// subtype error_max_turns. That ending is the M2 success path, not a failure.
func TestRunTreatsToolUseMaxTurnsAsSuccess(t *testing.T) {
	dir := t.TempDir()
	fixture, err := filepath.Abs(filepath.Join("testdata", "m2_single.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "fake-claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ncat >/dev/null\ncat "+fixture+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Bin: bin, ConfigDir: dir, Timeout: 10 * time.Second}
	req := Request{Model: "claude-haiku-5-5", MaxTokens: 1024,
		Tools:    json.RawMessage(`[{"name":"get_weather","input_schema":{"type":"object"}}]`),
		Messages: []Message{{Role: "user", Content: json.RawMessage(`"Weather in Paris?"`)}}}
	sw := &StreamWriter{}
	res, err := r.Run(context.Background(), req, "haiku", sw.Handle)
	if err != nil {
		t.Fatalf("Run = %v, want success for a tool_use ending", err)
	}
	if res.StopReason != "tool_use" {
		t.Errorf("StopReason = %q, want tool_use", res.StopReason)
	}
	if got := sw.Message(req.Model)["stop_reason"]; got != "tool_use" {
		t.Errorf("message stop_reason = %v, want tool_use", got)
	}
}

// The recorded round-trip: native acknowledges the replayed user frame with a
// zero-turn result, then answers from the tool_result.
func TestRunReplaysToolHistory(t *testing.T) {
	dir := t.TempDir()
	fixture, err := filepath.Abs(filepath.Join("testdata", "m2_roundtrip.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "fake-claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ncat "+fixture+"\ncat >/dev/null\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	req, err := ParseRequest([]byte(toolHistory))
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{Bin: bin, ConfigDir: dir, Timeout: 10 * time.Second}
	sw := &StreamWriter{}
	if _, err := r.Run(context.Background(), req, "haiku", sw.Handle); err != nil {
		t.Fatalf("Run = %v", err)
	}
	msg := sw.Message(req.Model)
	blocks, _ := msg["content"].([]map[string]any)
	if len(blocks) != 1 || !strings.Contains(fmt.Sprint(blocks[0]["text"]), "17") {
		t.Errorf("content = %v, want the answer built from the tool_result", blocks)
	}
	if msg["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason = %v, want end_turn", msg["stop_reason"])
	}
}
