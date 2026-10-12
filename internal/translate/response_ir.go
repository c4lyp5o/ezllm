package translate

import (
	"fmt"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// ResponseIR is used only by cross-wire codecs. Native bodies bypass it.
// Identity and created time come from the source; generated identities/times
// (when a source lacks them) are per-response, never fixed fixture constants.
type ResponseIR struct {
	ID           string
	Created      int64
	Blocks       []Block
	Finish       FinishKind
	StopSequence *string
	Usage        UsageIR
	Error        *ErrorIR
}

type FinishKind string

const (
	FinishCompleted    FinishKind = "completed"
	FinishTools        FinishKind = "tool_calls"
	FinishLength       FinishKind = "length"
	FinishRefusal      FinishKind = "refusal"
	FinishStopSequence FinishKind = "stop_sequence"
	FinishFailed       FinishKind = "failed"
)

// UsageIR keeps presence distinct from zero; partial stream snapshots MERGE,
// never sum (message_delta output_tokens is cumulative). Input is the full
// inclusive input count for translated projections; cache split is omitted.
// Raw counters remain available for the explicit Messages convention.
type UsageIR struct {
	Input      *int
	Output     *int
	CacheRead  *int
	CacheWrite *int
	Reasoning  *int
}

func (u *UsageIR) Merge(v UsageIR) {
	if v.Input != nil {
		u.Input = v.Input
	}
	if v.Output != nil {
		u.Output = v.Output
	}
	if v.CacheRead != nil {
		u.CacheRead = v.CacheRead
	}
	if v.CacheWrite != nil {
		u.CacheWrite = v.CacheWrite
	}
	if v.Reasoning != nil {
		u.Reasoning = v.Reasoning
	}
}

// Messages input_tokens is vendor-specific. Reverse codecs MUST have an
// explicit convention; M4 persists it and M5 supplies it on the real route.
type UsageConvention string

const (
	UsageUnknown   UsageConvention = "auto"
	UsageExclusive UsageConvention = "anthropic"
	UsageInclusive UsageConvention = "total"
)

// UsageConventionOf maps the stored per-account fact onto the codec
// primitive. 'auto' (or unset) is NOT a convention: a cross-wire Messages
// destination must refuse rather than guess whether input_tokens already
// includes cache tokens, so callers treat an error as "cannot convert".
func UsageConventionOf(m provider.UsageConvention) (UsageConvention, error) {
	switch m {
	case provider.UsageConventionAnthropic:
		return UsageExclusive, nil
	case provider.UsageConventionTotal:
		return UsageInclusive, nil
	}
	return UsageUnknown, fmt.Errorf("translate: account messages usage convention is unknown — set usage_convention to anthropic or total")
}

type ErrorIR struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Code    any    `json:"code,omitempty"`
}

// CodecOptions has one narrowly-scoped compatibility mode. Legacy wrappers
// retain their characterized lossy policy/bytes; new conversions are strict.
type CodecOptions struct {
	Legacy        bool
	MessagesUsage UsageConvention
}

// InvalidUpstreamResponse distinguishes a bad successful envelope from a
// source request refusal. Proxy maps it to 502; it must never become {} success.
type InvalidUpstreamResponse struct{ Reason string }

func (e *InvalidUpstreamResponse) Error() string {
	return "translate: invalid upstream response: " + e.Reason
}
func invalidResponse(reason string) error { return &InvalidUpstreamResponse{Reason: reason} }
