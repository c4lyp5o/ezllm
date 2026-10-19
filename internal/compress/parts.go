package compress

// ── wire-neutral text parts ────────────────────────────────────────────────
//
// Engines judge TEXT, not wire shapes. Every client ezllm serves carries the
// same two kinds of editable text, laid out three different ways:
//
//	prose        chat/messages: string content, or {"type":"text"} blocks
//	             responses:     message items, {"type":"input_text"|"output_text"}
//	tool output  chat:      role:"tool" content (string or text blocks)
//	             messages:  {"type":"tool_result"} content (string or text blocks)
//	             responses: {"type":"function_call_output"} output (string or text blocks)
//
// editParts is the single walker over all of them, so an engine written once
// applies to every client in every shape. What it never yields — tool-call
// arguments, images, thinking blocks (they carry signatures), and any block
// type it does not name — is never editable by construction. Every key around
// an edited text (cache_control, ids, flags) is carried over unchanged.

type partKind int

const (
	partProse partKind = iota
	partToolOutput
)

// textPart is one editable text: its kind, the role of the message carrying
// it ("tool" for tool output), the resolved tool name and call id for tool
// output ("" when unknown), and the text itself.
type textPart struct {
	kind partKind
	role string
	tool string
	id   string
	text string
}

// textBlockTypes are the block types whose "text" field is plain text.
var textBlockTypes = map[string]bool{"text": true, "input_text": true, "output_text": true}

// editParts calls fn for every editable text in msg. fn returns the
// replacement and whether to use it. The result is msg itself when nothing
// changed, or a copy-on-write clone where only the edited containers are new.
func editParts(msg any, names map[string]string, fn func(textPart) (string, bool)) (any, bool) {
	mm, ok := msg.(map[string]any)
	if !ok {
		return msg, false
	}
	if t, _ := mm["type"].(string); t == "function_call_output" {
		callID, _ := mm["call_id"].(string)
		p := textPart{kind: partToolOutput, role: "tool", tool: names[callID], id: callID}
		if v, changed := editTextValue(mm["output"], p, fn); changed {
			return withKey(mm, "output", v), true
		}
		return msg, false
	}
	if t, _ := mm["type"].(string); t != "" && t != "message" {
		return msg, false // function_call, reasoning, …: never editable
	}
	role, _ := mm["role"].(string)
	p := textPart{kind: partProse, role: role}
	if role == "tool" {
		p.kind, p.id = partToolOutput, toolCallIDOf(mm)
		p.tool = names[p.id]
	}
	if s, isStr := mm["content"].(string); isStr {
		p.text = s
		if out, use := fn(p); use {
			return withKey(mm, "content", out), true
		}
		return msg, false
	}
	blocks, ok := mm["content"].([]any)
	if !ok {
		return msg, false
	}
	var cp []any
	for i, b := range blocks {
		bm, ok := b.(map[string]any)
		if !ok {
			continue
		}
		var nb any
		changed := false
		switch t, _ := bm["type"].(string); {
		case textBlockTypes[t]:
			if s, isStr := bm["text"].(string); isStr {
				bp := p
				bp.text = s
				if out, use := fn(bp); use {
					nb, changed = withKey(bm, "text", out), true
				}
			}
		case t == "tool_result":
			id, _ := bm["tool_use_id"].(string)
			tp := textPart{kind: partToolOutput, role: "tool", tool: names[id], id: id}
			if v, ch := editTextValue(bm["content"], tp, fn); ch {
				nb, changed = withKey(bm, "content", v), true
			}
		}
		if changed {
			if cp == nil {
				cp = make([]any, len(blocks))
				copy(cp, blocks)
			}
			cp[i] = nb
		}
	}
	if cp == nil {
		return msg, false
	}
	return withKey(mm, "content", cp), true
}

// editTextValue edits a value that is either a string or an array of text
// blocks (the two forms a tool result's content/output may take).
func editTextValue(v any, p textPart, fn func(textPart) (string, bool)) (any, bool) {
	if s, isStr := v.(string); isStr {
		p.text = s
		if out, use := fn(p); use {
			return out, true
		}
		return v, false
	}
	arr, ok := v.([]any)
	if !ok {
		return v, false
	}
	var cp []any
	for i, b := range arr {
		bm, ok := b.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := bm["type"].(string); !textBlockTypes[t] {
			continue
		}
		s, isStr := bm["text"].(string)
		if !isStr {
			continue
		}
		bp := p
		bp.text = s
		if out, use := fn(bp); use {
			if cp == nil {
				cp = make([]any, len(arr))
				copy(cp, arr)
			}
			cp[i] = withKey(bm, "text", out)
		}
	}
	if cp == nil {
		return v, false
	}
	return cp, true
}

// withKey returns a shallow copy of m with key set to v. The caller's map is
// never mutated: the original body must stay intact for every fail-open path.
func withKey(m map[string]any, key string, v any) map[string]any {
	cp := make(map[string]any, len(m))
	for k, x := range m {
		cp[k] = x
	}
	cp[key] = v
	return cp
}

// editAll applies editParts to every message.
func editAll(msgs []any, names map[string]string, fn func(textPart) (string, bool)) []any {
	out := make([]any, len(msgs))
	for i, m := range msgs {
		out[i], _ = editParts(m, names, fn)
	}
	return out
}
