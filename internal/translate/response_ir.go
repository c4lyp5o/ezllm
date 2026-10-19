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
// inclusive input count; the cache split rides alongside it and every client
// projection re-derives its own wire convention from those two facts (Messages
// emits input minus cache plus the split; OpenAI/Responses keep the inclusive
// total and fill the details object). Raw counters remain available for the
// explicit Messages convention.
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
// primitive. 'auto' (or unset) reads as exclusive: that is what auto has
// always meant to the ledger (docs/usage-convention-design.md D1), and real
// Anthropic accounts are exclusive. A mis-set inclusive account over-reports
// input on the client's usage exactly as it already does in the ledger; the
// NormalizeMessagesUsage guard keeps the opposite mistake from going
// negative. Only a value outside the vocabulary is an error.
func UsageConventionOf(m provider.UsageConvention) (UsageConvention, error) {
	switch m {
	case provider.UsageConventionAnthropic, provider.UsageConventionAuto, "":
		return UsageExclusive, nil
	case provider.UsageConventionTotal:
		return UsageInclusive, nil
	}
	return UsageUnknown, fmt.Errorf("translate: account messages usage convention %q is not one of auto|anthropic|total — fix usage_convention", m)
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
