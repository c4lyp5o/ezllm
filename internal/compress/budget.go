package compress

import "strconv"

// budget keeps the high-signal conversation envelope when a prompt exceeds a
// token budget. It never changes system instructions, the first user turn, the
// latest turn, or tool-call/result messages. Messages are whole units: this is
// deliberate because trimming inside tool calls or content blocks can make a
// request invalid. The stage is opt-in through options.max_tokens.
func budget(msgs []any, options map[string]any) []any {
	limit := optionInt(options, "max_tokens")
	if limit <= 0 {
		return msgs
	}

	keep := make([]bool, len(msgs))
	used := int64(0)
	mark := func(i int) {
		if i < 0 || i >= len(msgs) || keep[i] {
			return
		}
		keep[i] = true
		used += EstimateTokens(string(mustMarshal(msgs[i])))
	}

	// Protect the instruction envelope and the first user request.
	for i, msg := range msgs {
		role, _ := messageRole(msg)
		if role == "system" {
			mark(i)
		}
	}
	for i, msg := range msgs {
		role, _ := messageRole(msg)
		if role == "user" {
			mark(i)
			break
		}
	}

	// Protect the latest turn and walk backwards through recent history.
	for i := len(msgs) - 1; i >= 0; i-- {
		role, _ := messageRole(msgs[i])
		if role == "tool" || hasToolCalls(msgs[i]) {
			markToolGroup(msgs, keep, &used, i)
			continue
		}
		if !keep[i] && used+EstimateTokens(string(mustMarshal(msgs[i]))) <= int64(limit) {
			mark(i)
		}
	}
	if len(msgs) > 0 {
		mark(len(msgs) - 1)
	}

	out := make([]any, 0, len(msgs))
	for i, msg := range msgs {
		if keep[i] {
			out = append(out, msg)
		}
	}
	return out
}

func optionInt(options map[string]any, key string) int {
	v, ok := options[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case string:
		i, _ := strconv.Atoi(n)
		return i
	default:
		return 0
	}
}

func messageRole(msg any) (string, bool) {
	m, ok := msg.(map[string]any)
	if !ok {
		return "", false
	}
	role, ok := m["role"].(string)
	return role, ok
}

func hasToolCalls(msg any) bool {
	m, ok := msg.(map[string]any)
	if !ok {
		return false
	}
	v, ok := m["tool_calls"].([]any)
	return ok && len(v) > 0
}

func markToolGroup(msgs []any, keep []bool, used *int64, i int) {
	mark := func(j int) {
		if j < 0 || j >= len(msgs) || keep[j] {
			return
		}
		keep[j] = true
		*used += EstimateTokens(string(mustMarshal(msgs[j])))
	}
	mark(i)
	if role, _ := messageRole(msgs[i]); role != "tool" {
		return
	}
	// A tool result is paired with the immediately preceding assistant tool
	// call in standard OpenAI history. Preserve that carrier as a unit.
	for j := i - 1; j >= 0; j-- {
		if hasToolCalls(msgs[j]) {
			mark(j)
			return
		}
		if role, _ := messageRole(msgs[j]); role != "tool" {
			return
		}
	}
}
