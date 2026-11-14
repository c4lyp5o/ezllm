package agybridge

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testConversationID = "6b902bb7-d944-4c0e-a64d-2b8a8f1047f3"
const cacheAnswer = `{"content":"pong","tool_calls":[]}`
const cacheLookup = `{"content":null,"tool_calls":[{"name":"lookup","arguments":{"key":"probe"}}]}`
const cacheBase = `{"model":"gemini-3.8-flash-low","tools":[{"type":"function","function":{"name":"lookup","description":"Read a caller value","parameters":{"type":"object","properties":{"key":{"type":"string"}},"required":["key"]}}}],"messages":[{"role":"user","content":"start"}]}`

func cacheReq(t *testing.T, body string) Request {
	t.Helper()
	req, err := parseRequest([]byte(body), true)
	if err != nil {
		t.Fatal(err)
	}
	return req
}
func nextCacheBody(t *testing.T, assistant map[string]any, last ...map[string]any) string {
	t.Helper()
	var body map[string]any
	json.Unmarshal([]byte(cacheBase), &body)
	messages := body["messages"].([]any)
	messages = append(messages, assistant)
	for _, m := range last {
		messages = append(messages, m)
	}
	body["messages"] = messages
	b, _ := json.Marshal(body)
	return string(b)
}
func cacheStream(t *testing.T, id, payload string, usage Usage) string {
	t.Helper()
	init := `{"event":"init","init":{"permission_mode":"request-review"}}`
	var structured any
	if err := json.Unmarshal([]byte(payload), &structured); err != nil {
		t.Fatal(err)
	}
	result, _ := json.Marshal(map[string]any{"event": "result", "result": map[string]any{"conversation_id": id, "status": "SUCCESS", "response": payload, "structured_output": structured, "usage": usage}})
	return init + "\n" + string(result) + "\n"
}
func cacheRig(t *testing.T, firstPayload, secondPayload string) (*Runner, string) {
	t.Helper()
	dir := t.TempDir()
	first := Usage{InputTokens: 17756, OutputTokens: 19, TotalTokens: 17775}
	second := Usage{InputTokens: 20156, OutputTokens: 38, CacheRead: 15697, TotalTokens: 20194}
	os.WriteFile(filepath.Join(dir, "first.out"), []byte(cacheStream(t, testConversationID, firstPayload, first)), 0600)
	os.WriteFile(filepath.Join(dir, "second.out"), []byte(cacheStream(t, testConversationID, secondPayload, second)), 0600)
	// Real subprocesses read argv/stdin and expose native phase/workspace use.
	script := `#!/bin/sh
phase=first
while [ "$#" -gt 0 ]; do
 case "$1" in
 --conversation) phase=second; shift; printf '%s\n' "$1" >> '` + dir + `/ids' ;;
 esac
 shift
done
printf '%s\n' "$phase" >> '` + dir + `/spawns'
pwd >> '` + dir + `/workspaces'
cat > '` + dir + `/stdin-'"$phase"
cat '` + dir + `/'"$phase"'.out'
if [ "$phase" = second ] && [ -e '` + dir + `/linger-second' ]; then sleep 1; fi
`
	bin := filepath.Join(dir, "agy")
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Bin: bin, Home: dir, Timeout: 2 * time.Second, MaxConcurrent: 1, SessionCache: true}
	t.Cleanup(r.Close)
	return r, dir
}
func TestConversationCacheResumesAndReportsRequestUsage(t *testing.T) {
	r, dir := cacheRig(t, cacheAnswer, cacheAnswer)
	first := cacheReq(t, cacheBase)
	res, err := r.Run(context.Background(), first, nil)
	if err != nil {
		t.Fatal(err)
	}
	text, _, err := first.Adapter.decode(res)
	if err != nil {
		t.Fatal(err)
	}
	body := nextCacheBody(t, map[string]any{"role": "assistant", "content": text}, map[string]any{"role": "user", "content": "next"})
	next := cacheReq(t, body)
	res, err = r.Run(context.Background(), next, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Usage.InputTokens != 2400 || res.Usage.CacheRead != 15697 || res.Usage.OutputTokens != 19 || res.Usage.TotalTokens != 2419 {
		t.Fatalf("cumulative counters billed again: %+v", res.Usage)
	}
	if read(t, dir+"/spawns") != "first\nsecond\n" || read(t, dir+"/ids") != testConversationID+"\n" {
		t.Fatal("conversation did not resume")
	}
	paths := strings.Fields(read(t, dir+"/workspaces"))
	if len(paths) != 2 || paths[0] != paths[1] {
		t.Fatal("resume lost private workspace")
	}
	secondPrompt := recordedCachePrompt(t, dir+"/stdin-second")
	if strings.Contains(secondPrompt, `"start"`) || !strings.Contains(secondPrompt, `next`) {
		t.Fatalf("full history replayed instead of delta: %s", secondPrompt)
	}
	if r.CacheStats()["hits"] != uint64(1) {
		t.Fatal("reuse not reported")
	}
	r.Close()
	if _, err := os.Stat(paths[0]); !os.IsNotExist(err) {
		t.Fatal("workspace retained after close")
	}
}
func TestConversationCachePreservesCallerToolIDsAndResults(t *testing.T) {
	r, dir := cacheRig(t, cacheLookup, `{"content":"caller_value","tool_calls":[]}`)
	first := cacheReq(t, cacheBase)
	res, err := r.Run(context.Background(), first, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, calls, err := first.Adapter.decode(res)
	if err != nil || len(calls) != 1 {
		t.Fatalf("calls=%v err=%v", calls, err)
	}
	_, again, _ := first.Adapter.decode(res)
	if again[0].ID != calls[0].ID {
		t.Fatal("tool IDs regenerated after caching")
	}
	body := nextCacheBody(t, map[string]any{"role": "assistant", "content": nil, "tool_calls": calls}, map[string]any{"role": "tool", "tool_call_id": calls[0].ID, "content": "caller_value"})
	next := cacheReq(t, body)
	res, err = r.Run(context.Background(), next, nil)
	if err != nil {
		t.Fatal(err)
	}
	text, out, err := next.Adapter.decode(res)
	if err != nil || text != "caller_value" || len(out) != 0 {
		t.Fatalf("tool result not consumed: %q %v %v", text, out, err)
	}
	delta := recordedCachePrompt(t, dir+"/stdin-second")
	if !strings.Contains(delta, calls[0].ID) || !strings.Contains(delta, "caller_value") || !strings.Contains(delta, "lookup") || strings.Contains(delta, `"start"`) {
		t.Fatal("caller result linkage missing from continuation")
	}
}
func TestConversationCacheRejectsChangedHistoryAndConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(string) string
	}{
		{"history", func(s string) string { return strings.Replace(s, "start", "edited", 1) }},
		{"answer", func(s string) string { return strings.Replace(s, "pong", "different", 1) }},
		{"model", func(s string) string { return strings.Replace(s, "gemini-3.8-flash-low", "another-model", 1) }},
		{"tools", func(s string) string { return strings.Replace(s, "Read a caller value", "Different tool", 1) }},
		{"choice", func(s string) string { return strings.Replace(s, `"messages":`, `"tool_choice":"none","messages":`, 1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, dir := cacheRig(t, cacheAnswer, cacheAnswer)
			if _, err := r.Run(context.Background(), cacheReq(t, cacheBase), nil); err != nil {
				t.Fatal(err)
			}
			body := nextCacheBody(t, map[string]any{"role": "assistant", "content": "pong"}, map[string]any{"role": "user", "content": "next"})
			if _, err := r.Run(context.Background(), cacheReq(t, tc.change(body)), nil); err != nil {
				t.Fatal(err)
			}
			if read(t, dir+"/spawns") != "first\nfirst\n" {
				t.Fatal("changed request resumed unrelated context")
			}
		})
	}
}
func TestConversationCacheNeverRepairsFailedResume(t *testing.T) {
	for _, mode := range []string{"identity", "usage-reset", "invalid-envelope", "native-tool"} {
		t.Run(mode, func(t *testing.T) {
			r, dir := cacheRig(t, cacheAnswer, cacheAnswer)
			if _, err := r.Run(context.Background(), cacheReq(t, cacheBase), nil); err != nil {
				t.Fatal(err)
			}
			bad := cacheStream(t, testConversationID, cacheAnswer, Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2})
			switch mode {
			case "identity":
				bad = cacheStream(t, "b933be96-f1f8-40bd-bfec-7cc4525c606f", cacheAnswer, Usage{InputTokens: 20156, OutputTokens: 38, CacheRead: 15697, TotalTokens: 20194})
			case "invalid-envelope":
				bad = cacheStream(t, testConversationID, `{"content":null,"tool_calls":[]}`, Usage{InputTokens: 20156, OutputTokens: 38, CacheRead: 15697, TotalTokens: 20194})
			case "native-tool":
				bad = `{"event":"init","init":{"permission_mode":"request-review"}}` + "\n" + `{"event":"step_update","step_update":{"step_type":"tool","tool_name":"write_to_file"}}` + "\n"
			}
			os.WriteFile(dir+"/second.out", []byte(bad), 0600)
			next := cacheReq(t, nextCacheBody(t, map[string]any{"role": "assistant", "content": "pong"}, map[string]any{"role": "user", "content": "next"}))
			if _, err := r.Run(context.Background(), next, nil); err == nil {
				t.Fatal("bad native resume accepted")
			}
			if read(t, dir+"/spawns") != "first\nsecond\n" {
				t.Fatal("failed resume issued repair inference")
			}
			if r.CacheStats()["idle"] != 0 {
				t.Fatal("failed resume retained state")
			}
			for _, work := range strings.Fields(read(t, dir+"/workspaces")) {
				if _, err := os.Stat(work); !os.IsNotExist(err) {
					t.Fatal("failed workspace leaked")
				}
			}
		})
	}
}
func TestConversationCacheDefaultIsFresh(t *testing.T) {
	r, dir := cacheRig(t, cacheAnswer, cacheAnswer)
	r.SessionCache = false
	for i := 0; i < 2; i++ {
		if _, err := r.Run(context.Background(), cacheReq(t, cacheBase), nil); err != nil {
			t.Fatal(err)
		}
	}
	if read(t, dir+"/spawns") != "first\nfirst\n" || r.CacheStats()["idle"] != 0 {
		t.Fatal("default unexpectedly retained a conversation")
	}
}
func TestConversationCacheTimersCannotEvictBorrowedOrRenewedEntry(t *testing.T) {
	r, _ := cacheRig(t, cacheAnswer, cacheAnswer)
	if _, err := r.Run(context.Background(), cacheReq(t, cacheBase), nil); err != nil {
		t.Fatal(err)
	}
	next := cacheReq(t, nextCacheBody(t, map[string]any{"role": "assistant", "content": "pong"}, map[string]any{"role": "user", "content": "next"}))
	entry, _, err := r.takeConversation(cacheState(next))
	if err != nil || entry == nil {
		t.Fatalf("borrow failed: %v", err)
	}
	oldGeneration := entry.generation - 1
	other, _, err := r.takeConversation(cacheState(next))
	if err != nil || other != nil {
		t.Fatal("same native conversation leased twice")
	}
	r.expireConversation(entry, oldGeneration)
	if _, err := os.Stat(entry.work); err != nil {
		t.Fatal("borrowed workspace removed")
	}
	if !r.rememberConversation(entry) {
		t.Fatal("renewal failed")
	}
	r.expireConversation(entry, oldGeneration)
	if r.CacheStats()["idle"] != 1 {
		t.Fatal("stale timer evicted renewed entry")
	}
}
func TestConversationCacheIdleExpiryRemovesWorkspace(t *testing.T) {
	r, dir := cacheRig(t, cacheAnswer, cacheAnswer)
	r.CacheIdleTimeout = 10 * time.Millisecond
	if _, err := r.Run(context.Background(), cacheReq(t, cacheBase), nil); err != nil {
		t.Fatal(err)
	}
	work := strings.TrimSpace(read(t, dir+"/workspaces"))
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(work); os.IsNotExist(err) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("idle workspace did not expire")
}

func recordedCachePrompt(t *testing.T, path string) string {
	t.Helper()
	var v struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(read(t, path)), &v); err != nil {
		t.Fatal(err)
	}
	return v.Message.Content
}

func TestConversationCacheDoesNotShareChangedToolCallIDs(t *testing.T) {
	r, dir := cacheRig(t, cacheLookup, cacheAnswer)
	first := cacheReq(t, cacheBase)
	res, err := r.Run(context.Background(), first, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, calls, err := first.Adapter.decode(res)
	if err != nil {
		t.Fatal(err)
	}
	calls = append([]ToolCall(nil), calls...)
	calls[0].ID += "_changed"
	next := cacheReq(t, nextCacheBody(t, map[string]any{"role": "assistant", "content": nil, "tool_calls": calls}, map[string]any{"role": "tool", "tool_call_id": calls[0].ID, "content": "foreign_result"}))
	if _, err := r.Run(context.Background(), next, nil); err != nil {
		t.Fatal(err)
	}
	if read(t, dir+"/spawns") != "first\nfirst\n" {
		t.Fatal("changed tool-call IDs resumed another context")
	}
}
func TestConversationCacheEvictsOnCapacityAndBinaryChange(t *testing.T) {
	r, dir := cacheRig(t, cacheAnswer, cacheAnswer)
	if _, err := r.Run(context.Background(), cacheReq(t, cacheBase), nil); err != nil {
		t.Fatal(err)
	}
	firstWork := strings.TrimSpace(read(t, dir+"/workspaces"))
	edited := strings.Replace(cacheBase, "start", "different", 1)
	if _, err := r.Run(context.Background(), cacheReq(t, edited), nil); err != nil {
		t.Fatal(err)
	}
	if r.CacheStats()["idle"] != 1 {
		t.Fatal("idle cache exceeded concurrency bound")
	}
	if _, err := os.Stat(firstWork); !os.IsNotExist(err) {
		t.Fatal("evicted workspace leaked")
	}
	// Editing the CLI invalidates cached native state even when history matches.
	b, err := os.ReadFile(r.Bin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.Bin, append(b, []byte("\n# replacement\n")...), 0700); err != nil {
		t.Fatal(err)
	}
	body := nextCacheBody(t, map[string]any{"role": "assistant", "content": "pong"}, map[string]any{"role": "user", "content": "next"})
	body = strings.Replace(body, "start", "different", 1)
	if _, err := r.Run(context.Background(), cacheReq(t, body), nil); err != nil {
		t.Fatal(err)
	}
	if read(t, dir+"/spawns") != "first\nfirst\nfirst\n" {
		t.Fatal("changed CLI resumed stale native state")
	}
}

func TestConversationCacheDiscardsResultAfterDeadline(t *testing.T) {
	r, dir := cacheRig(t, cacheAnswer, cacheAnswer)
	if _, err := r.Run(context.Background(), cacheReq(t, cacheBase), nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/linger-second", nil, 0600); err != nil {
		t.Fatal(err)
	}
	r.Timeout = 20 * time.Millisecond
	next := cacheReq(t, nextCacheBody(t, map[string]any{"role": "assistant", "content": "pong"}, map[string]any{"role": "user", "content": "next"}))
	start := time.Now()
	if _, err := r.Run(context.Background(), next, nil); err == nil {
		t.Fatal("response completed after deadline was retained")
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("deadline failed to terminate the post-result native process")
	}

	if r.CacheStats()["idle"] != 0 {
		t.Fatal("expired caller retained native state")
	}
}
func TestConversationCacheErrorsReturnUsageDeltas(t *testing.T) {
	r, dir := cacheRig(t, cacheAnswer, cacheAnswer)
	if _, err := r.Run(context.Background(), cacheReq(t, cacheBase), nil); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(dir + "/second.out")
	if err != nil {
		t.Fatal(err)
	}
	b = []byte(strings.Replace(string(b), `"status":"SUCCESS"`, `"status":"ERROR","error":"quota exhausted"`, 1))
	if err := os.WriteFile(dir+"/second.out", b, 0600); err != nil {
		t.Fatal(err)
	}
	next := cacheReq(t, nextCacheBody(t, map[string]any{"role": "assistant", "content": "pong"}, map[string]any{"role": "user", "content": "next"}))
	res, err := r.Run(context.Background(), next, nil)
	if err == nil || res.Usage.InputTokens != 2400 || res.Usage.OutputTokens != 19 || res.Usage.CacheRead != 15697 {
		t.Fatalf("failed request returned cumulative spend: %+v %v", res.Usage, err)
	}
	if r.CacheStats()["idle"] != 0 {
		t.Fatal("native error retained a conversation")
	}
}

func TestConversationCacheInvalidResumeKeepsBilledUsage(t *testing.T) {
	native := Usage{InputTokens: 20156, OutputTokens: 38, CacheRead: 15697, TotalTokens: 20194}
	for _, mode := range []string{"identity", "usage-reset"} {
		t.Run(mode, func(t *testing.T) {
			r, dir := cacheRig(t, cacheAnswer, cacheAnswer)
			if _, err := r.Run(context.Background(), cacheReq(t, cacheBase), nil); err != nil {
				t.Fatal(err)
			}
			want := native
			bad := cacheStream(t, "b933be96-f1f8-40bd-bfec-7cc4525c606f", cacheAnswer, native)
			if mode == "usage-reset" {
				want = Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}
				bad = cacheStream(t, testConversationID, cacheAnswer, want)
			}
			os.WriteFile(dir+"/second.out", []byte(bad), 0600)
			next := cacheReq(t, nextCacheBody(t, map[string]any{"role": "assistant", "content": "pong"}, map[string]any{"role": "user", "content": "next"}))
			res, err := r.Run(context.Background(), next, nil)
			if err == nil {
				t.Fatal("invalid native resume accepted")
			}
			if res.Usage != want {
				t.Fatalf("billed generation usage = %+v, want %+v", res.Usage, want)
			}
		})
	}
}
