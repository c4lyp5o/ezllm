package compress

// sessionDedup removes later byte-identical duplicates from history
// (re-sent system prompts — the 92.5% documented case).
//
// Droppable: system messages with string or pure-text-array content and NO
// tool_calls. Never dropped: user and assistant turns (a repeated "continue"
// or "yes" between assistant replies is a real conversation turn — dropping it
// hands the provider an assistant reply with no preceding question), role
// "tool" (breaks pairing — rtk covers those), tool_calls carriers, non-text
// blocks (tool_result, images), and the final message (handled by Apply's
// exempt slice).
//
// Dropping user/assistant repetition needs its own opt-in stage plus a test
// pinning the conversation shape after the rewrite; it is NOT the documented
// 92.5% case and must not ride along with system dedup.
//
// A dropped message becomes a nil placeholder rather than vanishing, so every
// engine keeps positions aligned and the pipeline knows exactly which message
// shipped as nothing (it commits that, too). The pipeline removes the nils
// when it assembles the body.
func sessionDedup(msgs []any) []any {
	seen := make(map[string]bool, len(msgs))
	out := make([]any, 0, len(msgs))
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			out = append(out, m)
			continue
		}
		role, _ := mm["role"].(string)
		switch role {
		case "system":
		default:
			out = append(out, m) // user/assistant/tool/other: never drop
			continue
		}
		if _, has := mm["tool_calls"]; has {
			out = append(out, m) // pairing carrier: never drop
			continue
		}
		key := hashContent(role, mm["content"])
		if key == "" {
			out = append(out, m) // non-text shape: not safe to hash-drop
			continue
		}
		if seen[key] {
			out = append(out, nil) // exact duplicate of an earlier block: drop
			continue
		}
		seen[key] = true
		out = append(out, m)
	}
	return out
}
