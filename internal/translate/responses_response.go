package translate

import "github.com/c4lyp5o/ezllm/internal/provider"

// ResponsesToChat translates a non-streaming OpenAI Responses API response
// body into an OpenAI chat-completions response body, for a chat client whose
// hop was dialed on the responses wire.
//
// Reasoning items are dropped silently (a chat body has no field for them);
// the envelope always carries a valid finish_reason; usage is remapped 1:1.
// Contract tables: docs/responses-wire-design.md.
func ResponsesToChat(body []byte, clientModel string) ([]byte, error) {
	opts := CodecOptions{Legacy: true}
	ir, err := DecodeResponse(body, provider.SurfaceResponses, opts)
	if err != nil {
		return nil, err
	}
	return EncodeResponse(ir, provider.SurfaceChat, clientModel, opts)
}

// ── responses wire decode types ───────────────────────────────────────────

type responsesWireResponse struct {
	ID         string               `json:"id"`
	CreatedAt  int64                `json:"created_at"`
	Model      string               `json:"model"`
	Status     string               `json:"status"`
	Incomplete *responsesIncomplete `json:"incomplete_details,omitempty"`
	Output     []responsesOutItem   `json:"output"`
	Usage      responsesOutUsage    `json:"usage"`
	Error      *responsesWireError  `json:"error,omitempty"`
}

type responsesIncomplete struct {
	Reason string `json:"reason"`
}

type responsesOutItem struct {
	Type      string                `json:"type"`
	Role      string                `json:"role,omitempty"`
	Content   []responsesOutContent `json:"content,omitempty"`
	CallID    string                `json:"call_id,omitempty"`
	Name      string                `json:"name,omitempty"`
	Arguments string                `json:"arguments,omitempty"`
}

type responsesOutContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type responsesOutUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type responsesWireError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code,omitempty"`
}

// ── chat wire output types (fixed field order) ────────────────────────────

type chatWireOutCompletion struct {
	ID      string              `json:"id"`
	Object  string              `json:"object"`
	Created int64               `json:"created"`
	Model   string              `json:"model"`
	Choices []chatWireOutChoice `json:"choices"`
	Usage   chatWireOutUsage    `json:"usage"`
}

type chatWireOutChoice struct {
	Index        int                `json:"index"`
	Message      chatWireOutMessage `json:"message"`
	FinishReason string             `json:"finish_reason"`
}

type chatWireOutMessage struct {
	Role      string                `json:"role"`
	Content   string                `json:"content,omitempty"`
	ToolCalls []chatWireOutToolCall `json:"tool_calls,omitempty"`
	// ReasoningContent is the de-facto chat field for visible reasoning
	// (DeepSeek's name, read by Hermes and most chat clients).
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

type chatWireOutToolCall struct {
	ID       string              `json:"id"`
	Type     string              `json:"type"`
	Function chatWireOutFunction `json:"function"`
}

type chatWireOutFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatWireOutUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}
