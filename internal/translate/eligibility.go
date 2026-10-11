package translate

import "github.com/c4lyp5o/ezllm/internal/provider"

// Eligible reports whether a hop may serve a translated request:
// translation must be enabled, the client must be speaking the anthropic
// surface, the hop must be PROVEN to speak the OpenAI shape, and the hop
// must NOT be proven to speak the native anthropic path (a native-capable
// hop never needs translation).
//
// NULL capability flags mean UNTESTED, and an untested hop is never
// translation-eligible: translation only rides on proven facts
// (proto_openai=true), which is what keeps the feature inert until a
// probe sweep marks models. protoAnthropic may be NULL — the native path
// being merely untested does not disqualify a hop we KNOW serves the
// OpenAI shape.
func Eligible(clientSurface provider.Surface, protoAnthropic, protoOpenAI *bool, enabled bool) bool {
	if !enabled || clientSurface != provider.SurfaceAnthropic {
		return false
	}
	if protoOpenAI == nil || !*protoOpenAI {
		return false
	}
	if protoAnthropic != nil && *protoAnthropic {
		return false
	}
	return true
}
