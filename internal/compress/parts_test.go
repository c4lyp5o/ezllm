package compress

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// One conversation, three wire shapes: chat (Hermes on /v1/chat/completions),
// messages (Claude Code on /v1/messages) and responses (Hermes on
// /v1/responses). It carries every kind of editable text — verbose prose,
// a shell log, a homogeneous JSON tool payload, whitespace-padded prose —
// plus a short final turn. Every engine must save the SAME amount and produce
// the SAME edited text in every shape: savings follow content, never the app.

const shapeProse = "I was wondering if you could basically look at the build because it seems very wrong.  \n\n\n\nThanks."

func shapeLog() string {
	var b strings.Builder
	for i := 0; i < 80; i++ {
		fmt.Fprintf(&b, "compiling module %d ok\n", i)
	}
	return b.String() + "error: module 81 failed\ndone"
}

func shapeTable() string {
	rows := make([]string, 0, 12)
	for i := 0; i < 12; i++ {
		rows = append(rows, fmt.Sprintf(`{"id":%d,"name":"pod-%d","status":"Running"}`, i, i))
	}
	return "[" + strings.Join(rows, ",") + "]"
}

func shapeBodies() map[string][]byte {
	log, table := shapeLog(), shapeTable()
	chat := []any{
		map[string]any{"role": "user", "content": shapeProse},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
			map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "bash", "arguments": `{"cmd":"make"}`}},
			map[string]any{"id": "c2", "type": "function", "function": map[string]any{"name": "kubectl_get", "arguments": `{}`}}}},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": log},
		map[string]any{"role": "tool", "tool_call_id": "c2", "content": table},
		map[string]any{"role": "user", "content": "go"},
	}
	messages := []any{
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": shapeProse}}},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "thinking", "thinking": "plan", "signature": "sig"},
			map[string]any{"type": "tool_use", "id": "c1", "name": "bash", "input": map[string]any{"cmd": "make"}},
			map[string]any{"type": "tool_use", "id": "c2", "name": "kubectl_get", "input": map[string]any{}}}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "c1", "content": log},
			map[string]any{"type": "tool_result", "tool_use_id": "c2", "content": []any{map[string]any{"type": "text", "text": table}},
				"cache_control": map[string]any{"type": "ephemeral"}}}},
		map[string]any{"role": "user", "content": "go"},
	}
	responses := []any{
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": shapeProse}}},
		map[string]any{"type": "function_call", "call_id": "c1", "name": "bash", "arguments": `{"cmd":"make"}`},
		map[string]any{"type": "function_call", "call_id": "c2", "name": "kubectl_get", "arguments": `{}`},
		map[string]any{"type": "function_call_output", "call_id": "c1", "output": log},
		map[string]any{"type": "function_call_output", "call_id": "c2", "output": table},
		map[string]any{"type": "message", "role": "user", "content": "go"},
	}
	mk := func(key string, list []any) []byte {
		b, _ := json.Marshal(map[string]any{"model": "m", key: list})
		return b
	}
	return map[string][]byte{"chat": mk("messages", chat), "messages": mk("messages", messages), "responses": mk("input", responses)}
}

// editedTexts lists every editable text of a body in walk order, so the three
// shapes can be compared text for text.
func editedTexts(t *testing.T, body []byte) []string {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatal(err)
	}
	list, _ := root["messages"].([]any)
	if list == nil {
		list, _ = root["input"].([]any)
	}
	var out []string
	for _, m := range list {
		editParts(m, nil, func(p textPart) (string, bool) {
			out = append(out, p.text)
			return "", false
		})
	}
	return out
}

func TestEveryEngineSavesTheSameInEveryShape(t *testing.T) {
	bodies := shapeBodies()
	for _, st := range []Stage{
		{Engine: "rtk"}, {Engine: "headroom"}, {Engine: "lite"},
		{Engine: "caveman", Options: map[string]any{"intensity": "standard"}},
	} {
		p := &Profile{Name: "p", Enabled: true, ExemptLastTurn: true, MinCompressRatio: 0, Stages: []Stage{st}}
		var refTexts []string
		var refSaved int64 = -1
		for _, shape := range []string{"chat", "messages", "responses"} {
			out, res := ApplyWithSession(bodies[shape], p, nil, "", nil)
			if !res.Applied {
				t.Errorf("%s/%s: not applied (%s)", st.Engine, shape, res.Err)
				continue
			}
			texts := editedTexts(t, out)
			if refTexts == nil {
				refTexts = texts
				refSaved = res.Stages[0].Saved
				continue
			}
			if strings.Join(texts, "\x00") != strings.Join(refTexts, "\x00") {
				t.Errorf("%s: %s edited text differs from chat:\n got %q\nwant %q", st.Engine, shape, texts, refTexts)
			}
			if res.Stages[0].Saved <= 0 || refSaved <= 0 {
				t.Errorf("%s/%s: stage saved %d (chat %d), want both > 0", st.Engine, shape, res.Stages[0].Saved, refSaved)
			}
		}
	}
}

// What the walker never yields is never editable: tool-call arguments,
// thinking blocks (signed), and the keys around an edited text survive
// byte-identical, cache_control included.
func TestPartsLeaveNonTextUntouched(t *testing.T) {
	p := &Profile{Name: "p", Enabled: true, ExemptLastTurn: true, Stages: []Stage{{Engine: "rtk"}, {Engine: "headroom"}, {Engine: "lite"}}}
	for shape, body := range shapeBodies() {
		out, res := ApplyWithSession(body, p, nil, "", nil)
		if !res.Applied {
			t.Fatalf("%s: not applied (%s)", shape, res.Err)
		}
		s := string(out)
		for _, must := range []string{`"arguments":"{\"cmd\":\"make\"}"`, `"input":{"cmd":"make"}`, `"signature":"sig"`, `"cache_control":{"type":"ephemeral"}`} {
			if strings.Contains(string(body), must) && !strings.Contains(s, must) {
				t.Errorf("%s: %s did not survive", shape, must)
			}
		}
		if !strings.Contains(s, "error: module 81 failed") {
			t.Errorf("%s: rtk dropped the error line", shape)
		}
	}
}

// A file read reaches the model byte-identical in every shape, through every
// engine: the agent's next edit quotes this text back, and a trimmed space or
// collapsed blank line makes that edit fail to match. Unknown tools still get
// the lossless tidy-up; a profile's "preserve" override protects them too.
func TestFileReadsStayVerbatimThroughEveryEngine(t *testing.T) {
	fileText := "     1\tfunc main() {   \n     2\t\n     3\t\n     4\t\n     5\t\tfmt.Println(\"hi\")  \n     6\t}\n"
	jsonFile := shapeTable()
	unknown := "status: ok   \n\n\n\n\ndone   "
	p := &Profile{Name: "p", Enabled: true, ExemptLastTurn: true, MinCompressRatio: 0, Stages: []Stage{
		{Engine: "rtk"}, {Engine: "headroom"}, {Engine: "lite"}, {Engine: "caveman", Options: map[string]any{"intensity": "standard"}}}}
	mk := func(results map[string]string) map[string][]byte {
		var chat, msgs, resp []any
		var uses, blocks []any
		for id, text := range results {
			name := map[string]string{"f": "read_file", "j": "read_file", "u": "deploy_status"}[id]
			chat = append(chat,
				map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": "{}"}}}},
				map[string]any{"role": "tool", "tool_call_id": id, "content": text})
			uses = append(uses, map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}})
			blocks = append(blocks, map[string]any{"type": "tool_result", "tool_use_id": id, "content": text})
			resp = append(resp,
				map[string]any{"type": "function_call", "call_id": id, "name": name, "arguments": "{}"},
				map[string]any{"type": "function_call_output", "call_id": id, "output": text})
		}
		msgs = []any{map[string]any{"role": "assistant", "content": uses}, map[string]any{"role": "user", "content": blocks}}
		end := map[string]any{"role": "user", "content": "go"}
		b := func(key string, list []any) []byte {
			out, _ := json.Marshal(map[string]any{"model": "m", key: append(list, end)})
			return out
		}
		return map[string][]byte{"chat": b("messages", chat), "messages": b("messages", msgs), "responses": b("input", resp)}
	}
	for shape, body := range mk(map[string]string{"f": fileText, "j": jsonFile, "u": unknown}) {
		out, _ := ApplyWithSession(body, p, nil, "", nil)
		texts := strings.Join(editedTexts(t, out), "\x00")
		if !strings.Contains(texts, fileText) {
			t.Errorf("%s: a file read was rewritten", shape)
		}
		if !strings.Contains(texts, jsonFile) {
			t.Errorf("%s: a JSON file read was rewritten (headroom)", shape)
		}
		if strings.Contains(texts, unknown) {
			t.Errorf("%s: an unknown tool's whitespace was not tidied", shape)
		}
	}
	// The profile says deploy_status is verbatim: no engine touches it.
	p.Stages[0].Options = map[string]any{"tool_classes": map[string]any{"deploy_status": "preserve"}}
	for shape, body := range mk(map[string]string{"u": unknown}) {
		out, _ := ApplyWithSession(body, p, nil, "", nil)
		if !strings.Contains(strings.Join(editedTexts(t, out), "\x00"), unknown) {
			t.Errorf("%s: a preserve override did not reach lite", shape)
		}
	}
}
