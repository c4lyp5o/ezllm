package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/c4lyp5o/ezllm/internal/compress"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// ── POST /admin/compression-preview ─────────────────────────────────────────
//
// Runs the REAL engines over a pasted sample so an operator can see what a
// pipeline does before attaching it to a combo. The profile is either a saved
// one (profile_id) or an unsaved draft sent inline (profile), so the editor
// can preview what it is about to save. Nothing is stored, nothing is sent
// upstream, and nothing reaches the ledger. The run is stateless (no session
// memo) and judged by the same floors as live traffic, unless the caller asks
// to ignore them to see what the engines alone can do.

// previewMaxText bounds the sample: the engines are linear in it, and a
// paste larger than this is not a sample.
const previewMaxText = 256 << 10

type previewProfile struct {
	Stages            json.RawMessage `json:"stages"`
	ExemptLastTurn    *bool           `json:"exempt_last_turn"`
	MinCompressRatio  *float64        `json:"min_compress_ratio"`
	AutoTriggerTokens *int64          `json:"auto_trigger_tokens"`
	FreshToolOutput   *string         `json:"fresh_tool_output"`
}

type previewRequest struct {
	ProfileID    *int64          `json:"profile_id"`
	Profile      *previewProfile `json:"profile"`
	Text         string          `json:"text"`
	Kind         string          `json:"kind"` // "tool_output" (default) | "prompt"
	Tool         string          `json:"tool"` // the tool's name, for tool_output: it decides how rtk treats the text
	IgnoreFloors bool            `json:"ignore_floors"`
}

// previewBody wraps the sample in the smallest conversation the engines treat
// as real work: the sample is never the final message (that one is exempt),
// and a tool result carries the call that produced it, because the tool's name
// is what decides whether it is filtered or preserved.
func previewBody(kind, tool, text string) (body []byte, at int) {
	var msgs []map[string]any
	if kind == "prompt" {
		msgs = []map[string]any{
			{"role": "system", "content": "You are a helpful assistant."},
			{"role": "user", "content": text},
			{"role": "assistant", "content": "Understood."},
			{"role": "user", "content": "Continue."},
		}
		at = 1
	} else {
		msgs = []map[string]any{
			{"role": "user", "content": "Run it."},
			{"role": "assistant", "content": nil, "tool_calls": []map[string]any{
				{"id": "call_preview", "type": "function", "function": map[string]any{"name": tool, "arguments": "{}"}},
			}},
			{"role": "tool", "tool_call_id": "call_preview", "content": text},
			{"role": "user", "content": "What does this show?"},
		}
		at = 2
	}
	body, _ = json.Marshal(map[string]any{"model": "preview", "messages": msgs})
	return body, at
}

// messageText reads a message's text back out of a body the pipeline returned:
// a plain string, or the text parts of a content array.
func messageText(body []byte, at int) string {
	var root struct {
		Messages []struct {
			Content any `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &root) != nil || at >= len(root.Messages) {
		return ""
	}
	switch c := root.Messages[at].Content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, p := range c {
			if m, ok := p.(map[string]any); ok {
				if t, ok := m["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func (s *Server) handleCompressionPreview(w http.ResponseWriter, r *http.Request) {
	var in previewRequest
	if !decodeBody(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.Text) == "" {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "text is required")
		return
	}
	if len(in.Text) > previewMaxText {
		writeErr(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "sample is too large: keep it under 256 KiB")
		return
	}
	if in.Kind == "" {
		in.Kind = "tool_output"
	}
	if in.Kind != "tool_output" && in.Kind != "prompt" {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", `kind must be "tool_output" or "prompt"`)
		return
	}
	if in.Tool == "" {
		in.Tool = "terminal"
	}

	// The profile: an inline draft wins over a saved id.
	p := store.CompressionProfile{
		Name: "preview", Enabled: true, ExemptLastTurn: true, MinCompressRatio: 0.05, FailOpen: true, FreshToolOutput: "keep",
	}
	switch {
	case in.Profile != nil:
		p.Stages = in.Profile.Stages
		if in.Profile.ExemptLastTurn != nil {
			p.ExemptLastTurn = *in.Profile.ExemptLastTurn
		}
		if in.Profile.MinCompressRatio != nil {
			p.MinCompressRatio = *in.Profile.MinCompressRatio
		}
		if in.Profile.AutoTriggerTokens != nil {
			p.AutoTriggerTokens = *in.Profile.AutoTriggerTokens
		}
		if in.Profile.FreshToolOutput != nil {
			p.FreshToolOutput = *in.Profile.FreshToolOutput
		}
	case in.ProfileID != nil:
		saved, err := s.db.GetCompressionProfile(r.Context(), *in.ProfileID)
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		p = *saved
		p.Enabled = true // a paused profile still previews: that is how it is judged before it is turned on
	default:
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "send profile_id or an inline profile")
		return
	}
	canonical, err := validateStages(p.Stages)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	p.Stages = canonical
	if err := checkProfileBounds(p); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	rt := compressProfileToRuntime(&p)
	rt.MinAbsoluteTokens = s.compressionMinAbs
	rt.MaxPipelineMS = s.compressionMaxMS
	if in.IgnoreFloors {
		rt.MinCompressRatio, rt.MinAbsoluteTokens = 0, 0
	}
	body, at := previewBody(in.Kind, in.Tool, in.Text)
	out, res := compress.ApplyWithSession(body, rt, nil, "", nil)

	after := in.Text
	if res.Applied {
		if t := messageText(out, at); t != "" {
			after = t
		}
	}
	stages := res.Stages
	if stages == nil {
		stages = []compress.StageStat{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"kind": in.Kind, "tool": in.Tool,
		"applied":        res.Applied,
		"tokens_before":  compress.EstimateTokens(in.Text),
		"tokens_after":   compress.EstimateTokens(after),
		"saved_notional": res.SavedNotional,
		"stages":         stages,
		"note":           res.Err,
		"after":          after,
	})
}
