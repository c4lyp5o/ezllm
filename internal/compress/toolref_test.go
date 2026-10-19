package compress

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// refConv builds a conversation in one wire shape: one tool call + result
// per (id, output) pair, in order, then a final user turn unless the last
// result should BE the final message.
func refConv(shape string, results [][2]string, tailIsTool bool) []byte {
	var list []any
	for _, r := range results {
		id, out := r[0], r[1]
		switch shape {
		case "chat":
			list = append(list,
				map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": id, "type": "function", "function": map[string]any{"name": "read_file", "arguments": "{}"}}}},
				map[string]any{"role": "tool", "tool_call_id": id, "content": out})
		case "messages":
			list = append(list,
				map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": id, "name": "Read", "input": map[string]any{}}}},
				map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": out}}})
		case "responses":
			list = append(list,
				map[string]any{"type": "function_call", "call_id": id, "name": "read_file", "arguments": "{}"},
				map[string]any{"type": "function_call_output", "call_id": id, "output": out})
		}
	}
	if !tailIsTool {
		if shape == "responses" {
			list = append(list, map[string]any{"type": "message", "role": "user", "content": "go"})
		} else {
			list = append(list, map[string]any{"role": "user", "content": "go"})
		}
	}
	key := "messages"
	if shape == "responses" {
		key = "input"
	}
	b, _ := json.Marshal(map[string]any{"model": "m", key: list})
	return b
}

var refShapes = []string{"chat", "messages", "responses"}

func refProfile(fresh bool, opts map[string]any) *Profile {
	return &Profile{Name: "r", Enabled: true, ExemptLastTurn: true, MinCompressRatio: 0, FreshToolOutput: fresh,
		Stages: []Stage{{Engine: "tool_ref", Options: opts}}}
}

func fileBody(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "%6d\tline %d of the file\n", i, i)
	}
	return b.String()
}

func refMarker(id string) string {
	return "[ezllm/ref: identical to the tool result for call " + id + " above]"
}

// A byte-identical later tool result becomes a reference to the first copy,
// which stays exactly where it was — for every tool (file reads included,
// the content is still above) and in every shape. Different output and
// output under min_bytes are never referenced.
func TestToolRefReplacesIdenticalLaterResults(t *testing.T) {
	file, other := fileBody(30), fileBody(31)
	for _, shape := range refShapes {
		body := refConv(shape, [][2]string{{"a", file}, {"b", other}, {"c", file}, {"d", file}, {"e", "tiny"}, {"f", "tiny"}}, false)
		out, res := Apply(body, refProfile(false, nil))
		got := editedTexts(t, out)
		want := []string{file, other, refMarker("a"), refMarker("a"), "tiny", "tiny", "go"}
		if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("%s:\n got %q\nwant %q (%s)", shape, got, want, res.Err)
		}
	}
	// min_bytes is the size floor (default 200).
	for _, shape := range refShapes {
		body := refConv(shape, [][2]string{{"a", file}, {"b", file}}, false)
		out, _ := Apply(body, refProfile(false, map[string]any{"min_bytes": len(file) + 1}))
		if strings.Contains(string(out), "ezllm/ref") {
			t.Errorf("%s: referenced an output below min_bytes", shape)
		}
	}
}

// Decided at first sight, frozen after: the shipped history is byte-stable
// across requests, so the provider's prefix cache keeps hitting.
func TestToolRefIsStableAcrossRequests(t *testing.T) {
	file := fileBody(30)
	for _, shape := range refShapes {
		memo := NewSessionMemo(nil)
		out1, _ := ApplyWithSession(refConv(shape, [][2]string{{"a", file}, {"b", file}}, false), refProfile(false, nil), nil, "auto:"+shape, memo)
		out2, _ := ApplyWithSession(refConv(shape, [][2]string{{"a", file}, {"b", file}, {"c", fileBody(5)}}, false), refProfile(false, nil), nil, "auto:"+shape, memo)
		t1, t2 := editedTexts(t, out1), editedTexts(t, out2)
		if t1[1] != refMarker("a") || t2[1] != refMarker("a") || t2[0] != file {
			t.Errorf("%s: shipped history changed between requests:\n1 %q\n2 %q", shape, t1, t2)
		}
	}
}

// If a client drops the referenced copy from its history, a frozen reference
// would point at nothing: that message ships its original content instead.
func TestToolRefNeverShipsADanglingReference(t *testing.T) {
	file := fileBody(30)
	for _, shape := range refShapes {
		memo := NewSessionMemo(nil)
		ApplyWithSession(refConv(shape, [][2]string{{"a", file}, {"b", file}}, false), refProfile(false, nil), nil, "auto:"+shape, memo)
		out, _ := ApplyWithSession(refConv(shape, [][2]string{{"b", file}, {"c", fileBody(5)}}, false), refProfile(false, nil), nil, "auto:"+shape, memo)
		if got := editedTexts(t, out); got[0] != file {
			t.Errorf("%s: shipped %q where the referenced copy is gone", shape, got[0])
		}
	}
}

// The final message is exempt unless fresh_tool_output: compress opens a
// purely tool-output final message — then it is referenced at first send.
func TestToolRefFinalMessageFollowsFreshToolOutput(t *testing.T) {
	file := fileBody(30)
	for _, shape := range refShapes {
		body := refConv(shape, [][2]string{{"a", file}, {"b", file}}, true)
		keep, _ := Apply(body, refProfile(false, nil))
		if strings.Contains(string(keep), "ezllm/ref") {
			t.Errorf("%s: keep referenced the exempt final message", shape)
		}
		comp, _ := Apply(body, refProfile(true, nil))
		if got := editedTexts(t, comp); got[len(got)-1] != refMarker("a") {
			t.Errorf("%s: compress did not reference the fresh final result: %q", shape, got[len(got)-1])
		}
	}
}
