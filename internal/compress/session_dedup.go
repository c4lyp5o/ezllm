package compress

// sessionDedup removes later byte-identical duplicates from history
// (re-sent system prompts, repeated text turns — the 92.5% documented case).
//
// Droppable: system/user/assistant messages with string or pure-text-array
// content and NO tool_calls. Never dropped: role:"tool" (breaks pairing —
// rtk covers those), tool_calls carriers, non-text blocks (tool_result,
// images), and the final message (handled by Apply's exempt slice).
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
		case "system", "user", "assistant":
		default:
			out = append(out, m) // tool/other: never drop
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
			continue // exact duplicate of an earlier block: drop
		}
		seen[key] = true
		out = append(out, m)
	}
	return out
}
