package codexbridge

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
)

// Canonical history follows the bridge's actual text/image interpretation.
// Cache markers and reasoning metadata are transport-only. Tool arguments keep
// numeric precision while normalizing JSON object order.
func messageCanonical(m chatMessage) ([]byte, error) {
	text, images, err := contentText(m.Content)
	if err != nil {
		return nil, err
	}
	calls := append([]chatToolCall(nil), m.ToolCalls...)
	for i := range calls {
		raw := calls[i].Function.Arguments
		dec := json.NewDecoder(bytes.NewBufferString(raw))
		dec.UseNumber()
		var obj any
		if json.Valid([]byte(raw)) && dec.Decode(&obj) == nil {
			if b, err := json.Marshal(obj); err == nil {
				calls[i].Function.Arguments = string(b)
			}
		}
	}
	return json.Marshal(struct {
		Role, Text string
		Images     []string
		Calls      []chatToolCall
		ToolID     string
	}{m.Role, text, images, calls, m.ToolCallID})
}
func extendHistory(prior [32]byte, m chatMessage) ([32]byte, error) {
	b, err := messageCanonical(m)
	if err != nil {
		return [32]byte{}, err
	}
	raw := make([]byte, 0, 32+len(b))
	raw = append(raw, prior[:]...)
	raw = append(raw, b...)
	return sha256.Sum256(raw), nil
}
func (r *Request) bindHistory(turns []chatMessage) error {
	toolStart := len(turns)
	for toolStart > 0 && turns[toolStart-1].Role == "tool" {
		toolStart--
	}
	var hash [32]byte
	for i, m := range turns {
		if i == toolStart {
			r.toolParentHash = hash
		}
		var err error
		hash, err = extendHistory(hash, m)
		if err != nil {
			return err
		}
	}
	r.hasHistory = true
	r.historyHash = hash
	return nil
}
func assistantHash(prior [32]byte, res Result) [32]byte {
	content, _ := json.Marshal(res.Text)
	m := chatMessage{Role: "assistant", Content: content}
	for _, call := range res.ToolCalls {
		tc := chatToolCall{ID: call.ID}
		tc.Function.Name = call.Name
		tc.Function.Arguments = call.Arguments
		m.ToolCalls = append(m.ToolCalls, tc)
	}
	hash, _ := extendHistory(prior, m)
	return hash
}
func inputHash(input []UserInput) [32]byte { b, _ := json.Marshal(input); return sha256.Sum256(b) }
