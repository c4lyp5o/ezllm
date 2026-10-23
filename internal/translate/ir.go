package translate

// The surface-neutral request model (M2).
//
// Three wires, one shape. Each surface has a decoder (its own JSON -> Request)
// and an encoder (Request -> its JSON), so N surfaces need N decoders + N
// encoders instead of N*(N-1) pairwise translators. Native traffic never
// touches this file: it stays byte-for-byte passthrough.
//
// The model is deliberately SMALL. It carries the portable subset that all
// three wires can express (text, ordered instructions, function tools, tool
// results, sampling controls), and refuses anything it cannot express honestly
// rather than dropping it silently — see UnsupportedFieldError.

import (
	"encoding/json"
	"fmt"
)

// Roles an IR turn can carry. "tool" is its own role because the chat wire
// needs exactly that shape (a tool message per result); the messages and
// responses encoders fold it into their own representation.
const (
	RoleSystem    = "system"
	RoleDeveloper = "developer"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// BlockKind discriminates Block. A flat struct (rather than an interface) keeps
// the model trivially comparable and copyable, which the failover path wants.
type BlockKind int

const (
	// BlockText is prose from/to the model.
	BlockText BlockKind = iota + 1
	// BlockCall is an assistant tool invocation: CallID + Name + Arguments
	// (a STRING of JSON, preserved verbatim from the source wire).
	BlockCall
	// BlockResult is a tool's output: CallID + Text.
	BlockResult
	// BlockImage is an image content element. ImageURL carries the resolved
	// URL — a data: URI for base64 payloads, or an http(s) URL otherwise —
	// so every wire can express it without a side channel.
	BlockImage
	// BlockReasoning is the model's visible reasoning (a Messages thinking
	// summary) in a response. Only the chat encoders carry it, as
	// reasoning_content; it is never sent back upstream.
	BlockReasoning
)

// Block is one content element of a turn.
type Block struct {
	Kind BlockKind
	// Text carries BlockText's prose and BlockResult's output.
	Text string
	// CallID identifies a call (BlockCall) or the call it answers
	// (BlockResult).
	CallID string
	// Name is the function name (BlockCall only).
	Name string
	// Arguments is the call's JSON argument STRING, verbatim as the source
	// wire had it (BlockCall only). Encoders that need an object parse it at
	// encode time and refuse malformed input instead of inventing "{}".
	Arguments string
	// ImageURL is the resolved image location (data: URI or http URL) for
	// BlockImage.
	ImageURL string
	// CacheControl is the client's prompt-cache breakpoint on this block, as
	// raw JSON ("" = none). Only the Messages encoder reads it; it is a
	// string so Block stays comparable.
	CacheControl string
}

// Turn is one message: a role plus ordered blocks.
type Turn struct {
	Role   string
	Blocks []Block
}

// Text returns the turn's concatenated prose (the chat wire's content).
func (t Turn) Text() string {
	out := ""
	for i, b := range t.Blocks {
		if b.Kind != BlockText {
			continue
		}
		if i > 0 && out != "" {
			out += "\n"
		}
		out += b.Text
	}
	return out
}

// Tool is a function tool definition. Schema bytes pass through UNCHANGED (no
// re-marshal): that is what keeps an upstream's prefix cache warm across a
// translated hop.
type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage
	Strict      *bool
}

// ToolChoice modes. "" means the source did not send one (the encoders then
// omit it — absent is not the same as "auto").
const (
	ToolChoiceAuto     = "auto"
	ToolChoiceNone     = "none"
	ToolChoiceRequired = "required" // chat's "required" / anthropic's "any"
	ToolChoiceTool     = "tool"     // one named function
)

// ToolChoice is the portable tool-selection instruction.
type ToolChoice struct {
	Mode string
	// Name is set only when Mode == ToolChoiceTool.
	Name string
}

// Request is the neutral request. Optional controls are POINTERS so "absent"
// and "zero" stay distinguishable end to end (temperature 0 is a real setting;
// a missing temperature must not become 0).
type RequestIR struct {
	Turns             []Turn
	Tools             []Tool
	ToolChoice        *ToolChoice
	MaxOutputTokens   *int
	Temperature       *float64
	TopP              *float64
	Stop              []string
	Stream            bool
	ParallelToolCalls *bool
	// ReasoningEffort is carried to the wires that have an equivalent (chat,
	// Responses). The Messages encoder drops it, as the legacy path drops
	// Anthropic thinking: the upstream's default effort applies.
	ReasoningEffort string
}

// UnsupportedFieldError reports a request the portable subset cannot carry.
// It exists so a cross-wire hop fails LOUDLY and specifically — naming the
// field and the reason — instead of forwarding a body that silently lost
// meaning. Legacy translators keep their documented drops; this is the new
// contract for new cells.
type UnsupportedFieldError struct {
	Field  string
	Reason string
}

func (e *UnsupportedFieldError) Error() string {
	return fmt.Sprintf("translate: unsupported %s: %s", e.Field, e.Reason)
}

func unsupported(field, reason string) error {
	return &UnsupportedFieldError{Field: field, Reason: reason}
}

// destSurface names an ENCODER destination (the wire we will dial).
type destSurface string

const (
	destChat      destSurface = "chat"
	destMessages  destSurface = "messages"
	destResponses destSurface = "responses"
)

// EncodeRequest is the one entry point the dispatcher uses: encode the IR for
// the destination wire. Nil IR is a programming error, not a client error.
func EncodeRequest(ir *RequestIR, dest destSurface, upstreamModel string) ([]byte, error) {
	if ir == nil {
		return nil, fmt.Errorf("translate: nil request IR for %s", dest)
	}
	switch dest {
	case destChat:
		return EncodeChatRequest(ir, upstreamModel)
	case destMessages:
		return EncodeMessagesRequest(ir, upstreamModel)
	case destResponses:
		return EncodeResponsesRequest(ir, upstreamModel)
	}
	return nil, fmt.Errorf("translate: unknown destination wire %q", dest)
}
