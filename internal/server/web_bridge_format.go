package server

import (
	"strings"
	"time"
)

// flattenWebBridgeMessages joins OpenAI messages into one prompt (the web protocol
// has no message array — a documented simplification, see the design doc).
// System messages are dropped: they carry the client's persona/memory
// (SOUL.md/MEMORY.md/USER.md) which is not part of the flattened prompt.
func flattenWebBridgeMessages(msgs []struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}) string {
	var parts []string
	for _, m := range msgs {
		// developer is the newer name for system (o-series and later clients):
		// both carry the persona/memory, which never goes to a web provider.
		if m.Role == "system" || m.Role == "developer" {
			continue // stealth: never forward persona/memory to web providers
		}
		if text := webBridgeContentText(m.Content); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n")
}

func webBridgeContentText(c any) string {
	switch v := c.(type) {
	case string:
		return v
	case []any:
		var sb strings.Builder
		for _, p := range v {
			if part, ok := p.(map[string]any); ok {
				if t, ok := part["text"].(string); ok {
					sb.WriteString(t)
					sb.WriteString("\n")
				}
			}
		}
		return strings.TrimSpace(sb.String())
	}
	return ""
}

func webBridgeChatResponse(id, model, text string) map[string]any {
	return map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": text},
			"finish_reason": "stop",
		}},
	}
}

// webBridgeHasImage reports an image part in any message. The web protocols
// take text only; dropping the image would answer a question the user did not
// ask, so the request is refused instead (the design's scope fence).
func webBridgeHasImage(msgs []struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}) bool {
	for _, m := range msgs {
		parts, ok := m.Content.([]any)
		if !ok {
			continue
		}
		for _, p := range parts {
			if part, ok := p.(map[string]any); ok {
				switch part["type"] {
				case "image_url", "image", "input_image":
					return true
				}
			}
		}
	}
	return false
}
