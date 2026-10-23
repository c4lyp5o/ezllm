package codexbridge

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func weatherRequest() Request {
	req := simpleRequest()
	req.Tools = []ToolSpec{{Name: "get_weather", Description: "weather", Parameters: json.RawMessage(`{"type":"object"}`)}}
	return req
}

func spawnCount(t *testing.T, home string) int {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(home, "spawns"))
	return len(strings.Fields(string(b)))
}

func pidAlive(pid int) bool {
	_, err := os.Stat("/proc/" + strconv.Itoa(pid))
	return err == nil
}

// Continuation (§4.6): the tool stop parks the child; the follow-up answers the
// pending call on the SAME child and the turn finishes from where it stopped.
func TestToolTurnParksAndResumesTheSameChild(t *testing.T) {
	r, home := fakeRunner(t, "tools")
	res, err := r.Run(context.Background(), weatherRequest(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0] != (ToolCall{ID: "call-a", Name: "get_weather", Arguments: `{"city":"KL"}`}) {
		t.Fatalf("tool calls = %+v", res.ToolCalls)
	}
	if res.Text != "Checking." || res.Usage.InputTokens != 4500 {
		t.Fatalf("segment text=%q usage=%+v", res.Text, res.Usage)
	}
	var ts map[string]any
	b, _ := os.ReadFile(filepath.Join(home, "thread_start.json"))
	_ = json.Unmarshal(b, &ts)
	if specs, _ := ts["dynamicTools"].([]any); len(specs) != 1 {
		t.Fatalf("dynamicTools not declared: %v", ts)
	}
	args, _, pid, _ := readStarted(t, home)
	for i, a := range args {
		if a == "code_mode_host" && args[i-1] == "--disable" {
			t.Fatal("code-mode host disabled on a tool-bearing request: the model could not call the tools")
		}
	}
	if r.ParkedTurns() != 1 || !pidAlive(pid) {
		t.Fatalf("parked=%d alive=%v, want the child parked", r.ParkedTurns(), pidAlive(pid))
	}

	follow := weatherRequest()
	follow.ToolResults = map[string]string{"call-a": "31C"}
	res, err = r.Run(context.Background(), follow, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "KL is 31C" || len(res.ToolCalls) != 0 {
		t.Fatalf("resumed text=%q calls=%v", res.Text, res.ToolCalls)
	}
	if res.Usage.InputTokens != 4500 || res.Usage.CachedTokens != 4000 {
		t.Fatalf("resumed usage = %+v, want only this segment's share", res.Usage)
	}
	if n := spawnCount(t, home); n != 1 {
		t.Fatalf("%d children spawned, want 1: the follow-up must resume, not replay", n)
	}
	if r.ParkedTurns() != 0 {
		t.Fatal("a finished turn stayed parked")
	}
	deadline := time.Now().Add(3 * time.Second)
	for pidAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatal("the finished child was not reaped")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Promise.all in the model's script: sibling calls come back as one batch.
func TestParallelToolCallsAreBatched(t *testing.T) {
	r, _ := fakeRunner(t, "tools_parallel")
	res, err := r.Run(context.Background(), weatherRequest(), nil)
	if err != nil || len(res.ToolCalls) != 2 {
		t.Fatalf("calls=%+v err=%v, want both in one stop", res.ToolCalls, err)
	}
	follow := weatherRequest()
	follow.ToolResults = map[string]string{"call-a": "31C", "call-b": "18C"}
	res, err = r.Run(context.Background(), follow, nil)
	if err != nil || res.Text != "KL 31C, Paris 18C" {
		t.Fatalf("text=%q err=%v", res.Text, err)
	}
}

// A script that awaits one result before making the next call stops twice.
func TestSequentialToolCallsStopTwice(t *testing.T) {
	r, home := fakeRunner(t, "tools_seq")
	res, err := r.Run(context.Background(), weatherRequest(), nil)
	if err != nil || len(res.ToolCalls) != 1 || res.ToolCalls[0].ID != "call-a" {
		t.Fatalf("first stop calls=%+v err=%v", res.ToolCalls, err)
	}
	follow := weatherRequest()
	follow.ToolResults = map[string]string{"call-a": "31C"}
	res, err = r.Run(context.Background(), follow, nil)
	if err != nil || len(res.ToolCalls) != 1 || res.ToolCalls[0].ID != "call-b" {
		t.Fatalf("second stop calls=%+v err=%v", res.ToolCalls, err)
	}
	follow.ToolResults = map[string]string{"call-a": "31C", "call-b": "18C"}
	res, err = r.Run(context.Background(), follow, nil)
	if err != nil || res.Text != "KL 31C then Paris 18C" || spawnCount(t, home) != 1 {
		t.Fatalf("text=%q err=%v spawns=%d", res.Text, err, spawnCount(t, home))
	}
}

// Results that match no parked turn (expired, evicted, restarted bridge) are
// replayed statelessly on a new child.
func TestUnmatchedToolResultsReplayStatelessly(t *testing.T) {
	r, home := fakeRunner(t, "ok")
	req := simpleRequest()
	req.ToolResults = map[string]string{"call-gone": "31C"}
	res, err := r.Run(context.Background(), req, nil)
	if err != nil || res.Text != "pong" || spawnCount(t, home) != 1 {
		t.Fatalf("text=%q err=%v spawns=%d", res.Text, err, spawnCount(t, home))
	}
}

func TestParkedTurnExpires(t *testing.T) {
	r, home := fakeRunner(t, "tools")
	r.ToolHold = 100 * time.Millisecond
	if _, err := r.Run(context.Background(), weatherRequest(), nil); err != nil {
		t.Fatal(err)
	}
	_, _, pid, _ := readStarted(t, home)
	deadline := time.Now().Add(3 * time.Second)
	for r.ParkedTurns() != 0 || pidAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("parked=%d alive=%v after the hold expired", r.ParkedTurns(), pidAlive(pid))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestMaxParkedEvictsTheOldest(t *testing.T) {
	r, home := fakeRunner(t, "tools")
	r.MaxParked = 1
	if _, err := r.Run(context.Background(), weatherRequest(), nil); err != nil {
		t.Fatal(err)
	}
	_, _, first, _ := readStarted(t, home)
	if _, err := r.Run(context.Background(), weatherRequest(), nil); err != nil {
		t.Fatal(err)
	}
	if r.ParkedTurns() != 1 {
		t.Fatalf("parked = %d, want 1", r.ParkedTurns())
	}
	deadline := time.Now().Add(3 * time.Second)
	for pidAlive(first) {
		if time.Now().After(deadline) {
			t.Fatal("the evicted child is still alive")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestExecChannelRefusesTools(t *testing.T) {
	r, _ := fakeRunner(t, "ok")
	r.Channel = ChannelExec
	_, err := r.Run(context.Background(), weatherRequest(), nil)
	if ne := asNative(err); ne.Status != 400 || ne.Code != "chat_tools_unsupported" {
		t.Fatalf("err = %v", err)
	}
}

func TestToolArguments(t *testing.T) {
	for raw, want := range map[string]string{
		`{"city":"KL"}`:       `{"city":"KL"}`,
		`"{\"city\":\"KL\"}"`: `{"city":"KL"}`,
		`"just text"`:         `"just text"`,
		``:                    `{}`,
	} {
		if got := toolArguments(json.RawMessage(raw)); got != want {
			t.Errorf("toolArguments(%s) = %s, want %s", raw, got, want)
		}
	}
}
