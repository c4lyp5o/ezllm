package compress

import (
	"encoding/json"
	"strings"
)

// Wire-neutral tool-call identity. A tool call and its result are linked by an
// id in every wire shape ezllm accepts; reading the id (never the shape) is
// what lets one rule — "a call and its result travel together" — hold for
// every client:
//
//	chat:      assistant tool_calls[].id           ↔ role:"tool" tool_call_id
//	messages:  assistant content tool_use .id      ↔ user content tool_result .tool_use_id

// toolIDsIssued returns the tool-call ids a message issues.
func toolIDsIssued(msg any) []string {
	mm, ok := msg.(map[string]any)
	if !ok {
		return nil
	}
	var ids []string
	if calls, ok := mm["tool_calls"].([]any); ok {
		for _, c := range calls {
			if cm, ok := c.(map[string]any); ok {
				if id, _ := cm["id"].(string); id != "" {
					ids = append(ids, id)
				}
			}
		}
	}
	for _, b := range contentBlocks(mm) {
		if t, _ := b["type"].(string); t == "tool_use" {
			if id, _ := b["id"].(string); id != "" {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// toolIDsAnswered returns the tool-call ids a message answers.
func toolIDsAnswered(msg any) []string {
	mm, ok := msg.(map[string]any)
	if !ok {
		return nil
	}
	var ids []string
	if id := toolCallIDOf(mm); id != "" {
		ids = append(ids, id)
	}
	for _, b := range contentBlocks(mm) {
		if t, _ := b["type"].(string); t == "tool_result" {
			if id, _ := b["tool_use_id"].(string); id != "" {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// isToolBearing reports whether a message issues or answers a tool call.
func isToolBearing(msg any) bool {
	return len(toolIDsIssued(msg)) > 0 || len(toolIDsAnswered(msg)) > 0
}

func contentBlocks(mm map[string]any) []map[string]any {
	arr, ok := mm["content"].([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(arr))
	for _, b := range arr {
		if bm, ok := b.(map[string]any); ok {
			out = append(out, bm)
		}
	}
	return out
}

// isJSONPayload reports whether text is a whole JSON object or array. Line
// filters must never edit such content (contract rule 2, "never mid-JSON"),
// whichever wire shape carried it.
func isJSONPayload(text string) bool {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[") {
		return false
	}
	return json.Valid([]byte(trimmed))
}
