package copilotbridge

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
)

// The existing worker owns at most one completed native conversation. Its idle,
// age and turn limits bound retention; no additional child or disk cache exists.
const maxSessionHistoryBytes = 1 << 20

type completedSession struct {
	config    [32]byte
	history   []json.RawMessage
	assistant json.RawMessage
	usage     Usage
}

func (r *Runner) SessionCacheEnabled() bool { return r.WarmPool && r.SessionCache }

func canonicalCacheHistory(body []byte) []json.RawMessage {
	if len(body) > maxSessionHistoryBytes {
		return nil
	}
	var envelope struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if json.Unmarshal(body, &envelope) != nil || len(envelope.Messages) == 0 {
		return nil
	}
	for i, raw := range envelope.Messages {
		var value any
		// UseNumber avoids rounding numeric tool arguments while comparing history.
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if dec.Decode(&value) != nil {
			return nil
		}
		message, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		// Hermes moves ephemeral markers as history grows. They are transport
		// metadata that this channel never sends to the model, not edits to
		// the conversation. Normalize text-block representation the same way.
		delete(message, "cache_control")
		if parts, ok := message["content"].([]any); ok {
			var text strings.Builder
			for _, part := range parts {
				block, ok := part.(map[string]any)
				if !ok {
					return nil
				}
				kind, _ := block["type"].(string)
				if kind != "text" && kind != "input_text" {
					return nil
				}
				value, ok := block["text"].(string)
				if !ok {
					return nil
				}
				text.WriteString(value)
			}
			message["content"] = text.String()
		}
		if calls, present := message["tool_calls"]; present {
			if list, ok := calls.([]any); calls == nil || (ok && len(list) == 0) {
				delete(message, "tool_calls")
			}
		}
		envelope.Messages[i], _ = json.Marshal(message)
	}
	return envelope.Messages
}
func sessionConfigHash(turn ChannelTurn) [32]byte {
	b, _ := json.Marshal(sessionConfig(turn))
	return sha256.Sum256(b)
}
func cacheEligible(turn ChannelTurn) bool {
	return turn.Model != "" && turn.Model != "auto" && len(turn.cacheHistory) > 0 && len(turn.Attachments) == 0
}
func (s *completedSession) continuation(turn ChannelTurn) (string, bool) {
	if s == nil || !cacheEligible(turn) || s.config != sessionConfigHash(turn) || len(turn.cacheHistory) != len(s.history)+2 {
		return "", false
	}
	for i, raw := range s.history {
		if !bytes.Equal(raw, turn.cacheHistory[i]) {
			return "", false
		}
	}
	if !bytes.Equal(s.assistant, turn.cacheHistory[len(s.history)]) {
		return "", false
	}
	var user struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(turn.cacheHistory[len(turn.cacheHistory)-1], &user) != nil || user.Role != "user" {
		return "", false
	}
	var images []ImageAttachment
	imageBytes := 0
	text, err := contentWithImages(user.Content, &images, &imageBytes)
	if err != nil || len(images) > 0 {
		return "", false
	}
	return "User: " + text, true
}
func completedFrom(turn ChannelTurn, res Result) *completedSession {
	assistant, _ := json.Marshal(map[string]any{"role": "assistant", "content": res.Text})
	size := len(assistant)
	for _, raw := range turn.cacheHistory {
		size += len(raw)
	}
	if size > maxSessionHistoryBytes || strings.TrimSpace(res.Text) == "" {
		return nil
	}
	return &completedSession{config: sessionConfigHash(turn), history: turn.cacheHistory, assistant: assistant, usage: res.Usage}
}

// Native checkpoints are cumulative within a retained session. Return only the
// new request's counters; an unexpected reset must never bill old work again.
func usageDelta(now, prior Usage) (Usage, bool) {
	delta := Usage{PromptTokens: now.PromptTokens - prior.PromptTokens, CompletionTokens: now.CompletionTokens - prior.CompletionTokens, CachedRead: now.CachedRead - prior.CachedRead, CachedWrite: now.CachedWrite - prior.CachedWrite, ReasoningTokens: now.ReasoningTokens - prior.ReasoningTokens, PremiumRequests: now.PremiumRequests - prior.PremiumRequests, NanoAIU: now.NanoAIU - prior.NanoAIU, Model: now.Model}
	ok := delta.PromptTokens >= 0 && delta.CompletionTokens >= 0 && delta.CachedRead >= 0 && delta.CachedWrite >= 0 && delta.ReasoningTokens >= 0 && delta.PremiumRequests >= 0 && delta.NanoAIU >= 0
	if !ok {
		return Usage{}, false
	}
	return delta, true
}
func (c *rpcConn) deleteSession(id string) bool {
	var deleted struct {
		Success bool `json:"success"`
	}
	var err error
	c.bounded(deleteTimeout, func() { err = c.call("session.delete", map[string]any{"sessionId": id}, &deleted) })
	if c.retired == nil {
		c.retired = make(map[string]bool)
	}
	c.retired[id] = true
	c.session = ""
	c.queue = nil
	c.pending = nil
	c.completed = nil
	return err == nil && deleted.Success
}

var errSessionUsageReset = errors.New("copilot-bridge: retained session usage counters regressed")
