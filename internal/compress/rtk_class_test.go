package compress

import (
	"strings"
	"testing"
)

// WS-E stage 1: rtk filters BY TOOL CLASS (feasibility audit 2026-10-16).
// The failure mode this kills: a 300-line file read or a patch being
// head/tail-truncated like build noise, and an UNKNOWN tool getting the
// same blunt treatment. Unknown must mean preserved.

func bigLog() string {
	var sb strings.Builder
	for i := 0; i < 60; i++ {
		sb.WriteString("noise line that goes on and on and on for a while yes\n")
	}
	return sb.String()
}

// log class (bash) is still filtered, with errors preserved.
func TestRTKLogClassStillFilters(t *testing.T) {
	content := bigLog() + "error: boom\n"
	body := bodyOf(
		toolCallMsg("call_b", "bash"),
		map[string]any{"role": "tool", "tool_call_id": "call_b", "content": content},
		textMsg("user", "and now?"), // the tool result must not be the exempt last turn
	)
	out, res := Apply(body, fullProfile(Stage{Engine: "rtk", Options: map[string]any{"keep_lines": 3}}))
	if !res.Applied {
		t.Fatalf("log class must filter: %s", res.Err)
	}
	got := msgContent(t, out, 1)
	if !strings.Contains(got, "error: boom") {
		t.Error("error line must survive")
	}
	if !strings.Contains(got, "[ezllm/rtk:") {
		t.Error("log class should elide the middle")
	}
}

// FILE READS are preserved verbatim — they are not terminal noise.
func TestRTKPreservesFileReads(t *testing.T) {
	content := bigLog() + "line 41 of the file you actually asked about\n"
	for _, name := range []string{"read_file", "read", "view", "str_replace_editor", "Read"} {
		body := bodyOf(
			toolCallMsg("call_r", name),
			map[string]any{"role": "tool", "tool_call_id": "call_r", "content": content},
		)
		out, res := Apply(body, fullProfile(Stage{Engine: "rtk"}))
		if res.Applied {
			t.Errorf("%s: a file read must never be head/tail-truncated", name)
		}
		if got := msgContent(t, out, 1); got != content {
			t.Errorf("%s: file read modified", name)
		}
	}
}

// DIFFS/PATCHES are preserved verbatim.
func TestRTKPreservesDiffsAndPatches(t *testing.T) {
	content := bigLog() + "diff --git a/x.go b/x.go\n@@ -1 +1 @@\n-old\n+new\n"
	for _, name := range []string{"apply_patch", "edit_file", "str_replace", "Write"} {
		body := bodyOf(
			toolCallMsg("call_d", name),
			map[string]any{"role": "tool", "tool_call_id": "call_d", "content": content},
		)
		out, res := Apply(body, fullProfile(Stage{Engine: "rtk"}))
		if res.Applied {
			t.Errorf("%s: a patch must never be truncated", name)
		}
		if got := msgContent(t, out, 1); got != content {
			t.Errorf("%s: patch modified", name)
		}
	}
}

// UNKNOWN tools and UNRESOLVABLE ids are preserved.
func TestRTKPreservesUnknownTools(t *testing.T) {
	content := bigLog()
	body := bodyOf(
		toolCallMsg("call_u", "mcp__acme__fetch_everything"),
		map[string]any{"role": "tool", "tool_call_id": "call_u", "content": content},
	)
	out, res := Apply(body, fullProfile(Stage{Engine: "rtk"}))
	if res.Applied {
		t.Error("an unknown tool must pass through untouched")
	}
	if got := msgContent(t, out, 1); got != content {
		t.Error("unknown tool result modified")
	}
	// A tool result with no resolvable call at all is also unknown.
	body2 := bodyOf(map[string]any{"role": "tool", "tool_call_id": "ghost", "content": content})
	out2, res2 := Apply(body2, fullProfile(Stage{Engine: "rtk"}))
	if res2.Applied || msgContent(t, out2, 0) != content {
		t.Error("a tool result with no originating call must pass through")
	}
}

// SEARCH class is filtered (ranked head/tail, paths survive).
func TestRTKSearchesFilter(t *testing.T) {
	content := bigLog() + "src/main.go:41: the answer\n"
	body := bodyOf(
		toolCallMsg("call_g", "grep"),
		map[string]any{"role": "tool", "tool_call_id": "call_g", "content": content},
		textMsg("user", "and now?"),
	)
	out, res := Apply(body, fullProfile(Stage{Engine: "rtk", Options: map[string]any{"keep_lines": 3}}))
	if !res.Applied {
		t.Fatalf("search results should filter: %s", res.Err)
	}
	got := msgContent(t, out, 1)
	if !strings.Contains(got, "src/main.go") {
		t.Error("search results must keep their paths/locations")
	}
}

// PARALLEL tool calls: pairing must survive, each class by its own name.
func TestRTKParallelCallsKeepPairing(t *testing.T) {
	log := bigLog() + "error: boom\n"
	body := bodyOf(
		map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
			map[string]any{"id": "p1", "type": "function", "function": map[string]any{"name": "bash", "arguments": "{}"}},
			map[string]any{"id": "p2", "type": "function", "function": map[string]any{"name": "read_file", "arguments": "{}"}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "p1", "content": log},
		map[string]any{"role": "tool", "tool_call_id": "p2", "content": log},
	)
	out, res := Apply(body, fullProfile(Stage{Engine: "rtk", Options: map[string]any{"keep_lines": 3}}))
	if !res.Applied {
		t.Fatalf("expected the bash half filtered: %s", res.Err)
	}
	msgs := decodeMsgs(t, out)
	bash, _ := msgs[1].(map[string]any)
	read, _ := msgs[2].(map[string]any)
	if b, _ := bash["content"].(string); !strings.Contains(b, "[ezllm/rtk:") {
		t.Error("the bash result must be filtered")
	}
	if r, _ := read["content"].(string); r != log {
		t.Error("the read_file result must be preserved verbatim")
	}
	if bash["tool_call_id"] != "p1" || read["tool_call_id"] != "p2" {
		t.Error("tool_call_id pairing must survive filtering")
	}
}

// ANTHROPIC wire shape: tool_use blocks + tool_use_id resolve the same way.
func TestRTKAnthropicShapeResolves(t *testing.T) {
	content := bigLog() + "error: boom\n"
	body := bodyOf(
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "toolu_1", "name": "bash", "input": map[string]any{"command": "make"}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": content},
		}},
		textMsg("user", "final"),
	)
	out, res := Apply(body, fullProfile(Stage{Engine: "rtk", Options: map[string]any{"keep_lines": 3}}))
	if !res.Applied {
		t.Fatalf("anthropic tool_use id must resolve (got %s)", res.Err)
	}
	blocks, _ := decodeMsgs(t, out)[1].(map[string]any)["content"].([]any)
	got, _ := blocks[0].(map[string]any)["content"].(string)
	if !strings.Contains(got, "error: boom") || !strings.Contains(got, "[ezllm/rtk:") {
		t.Errorf("anthropic result not filtered correctly: %q", got)
	}
}

// helpers

func toolCallMsg(id, name string) map[string]any {
	return map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
		map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": "{}"}},
	}}
}

func msgContent(t *testing.T, body []byte, i int) string {
	t.Helper()
	m, ok := decodeMsgs(t, body)[i].(map[string]any)
	if !ok {
		t.Fatalf("message %d is not an object", i)
	}
	s, _ := m["content"].(string)
	return s
}
