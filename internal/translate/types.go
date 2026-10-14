package translate

import "encoding/json"

// --- Anthropic Types ---

type AnthropicRequest struct {
	Model         string               `json:"model"`
	System        any                  `json:"system"` // string or []AnthropicBlock
	Messages      []AnthropicMessage   `json:"messages"`
	MaxTokens     int                  `json:"max_tokens"`
	StopSequences []string             `json:"stop_sequences,omitempty"`
	Temperature   *float64             `json:"temperature,omitempty"`
	TopP          *float64             `json:"top_p,omitempty"`
	Stream        bool                 `json:"stream"`
	Tools         []AnthropicTool      `json:"tools,omitempty"`
	ToolChoice    *AnthropicToolChoice `json:"tool_choice,omitempty"`
}

type AnthropicBlock struct {
	Type  string `json:"type"`
	Text  string `json:"text,omitempty"`
	ID    string `json:"id,omitempty"`
	Name  string `json:"name,omitempty"`
	Input any    `json:"input,omitempty"`
}

type AnthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string or []AnthropicBlock
}

type AnthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type AnthropicToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

type AnthropicResponse struct {
	ID           string           `json:"id"`
	Type         string           `json:"type"`
	Role         string           `json:"role"`
	Content      []AnthropicBlock `json:"content"`
	Model        string           `json:"model"`
	StopReason   string           `json:"stop_reason"`
	StopSequence *string          `json:"stop_sequence"`
	Usage        AnthropicUsage   `json:"usage"`
}

// AnthropicUsage is the legacy-path usage struct (encode via json.Marshal of
// the whole envelope). The cache fields are omitempty so an upstream that
// reports no split marshals to byte-identical legacy output; when present they
// carry the same split the strict projection emits, and InputTokens is then
// already exclusive of both — the Messages contract, not the vendor's total.
type AnthropicUsage struct {
	InputTokens  int  `json:"input_tokens"`
	OutputTokens int  `json:"output_tokens"`
	CacheRead    *int `json:"cache_read_input_tokens,omitempty"`
	CacheWrite   *int `json:"cache_creation_input_tokens,omitempty"`
}

// projectAnthropicUsage applies the Messages contract (input EXCLUDES cache,
// split carried alongside) to an inclusive IR usage. Shared by the strict and
// legacy encoders so the two cannot drift on the convention the ledger reads.
func projectAnthropicUsage(in *int, out *int, cacheRead *int, cacheWrite *int) AnthropicUsage {
	u := AnthropicUsage{CacheRead: cacheRead, CacheWrite: cacheWrite}
	if in != nil {
		total := *in
		for _, c := range []*int{cacheRead, cacheWrite} {
			if c != nil && *c > 0 {
				total -= *c
			}
		}
		if total < 0 {
			total = 0
		}
		u.InputTokens = total
	}
	if out != nil {
		u.OutputTokens = *out
	}
	return u
}

// --- OpenAI Types ---

type OpenAIRequest struct {
	Model         string               `json:"model"`
	Messages      []OpenAIMessage      `json:"messages"`
	MaxTokens     int                  `json:"max_tokens"`
	Stop          []string             `json:"stop,omitempty"`
	Temperature   *float64             `json:"temperature,omitempty"`
	TopP          *float64             `json:"top_p,omitempty"`
	Stream        bool                 `json:"stream,omitempty"`
	StreamOptions *OpenAIStreamOptions `json:"stream_options,omitempty"`
	Tools         []OpenAITool         `json:"tools,omitempty"`
	ToolChoice    any                  `json:"tool_choice,omitempty"` // string or ToolChoice
}

type OpenAIMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	ToolCalls  []OpenAIToolCall `json:"tool_calls,omitempty"`
}

type OpenAIStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type OpenAITool struct {
	Type     string         `json:"type"`
	Function OpenAIFunction `json:"function"`
}

type OpenAIFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Arguments   string          `json:"arguments,omitempty"`
}

type OpenAIToolCall struct {
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Function OpenAIFunction `json:"function"`
}

type OpenAIToolChoice struct {
	Type     string         `json:"type"`
	Function OpenAIFunction `json:"function"`
}

type OpenAIResponse struct {
	ID      string         `json:"id"`
	Choices []OpenAIChoice `json:"choices"`
	Usage   OpenAIUsage    `json:"usage"`
}

type OpenAIChoice struct {
	Message      OpenAIMessage `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

type OpenAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}
