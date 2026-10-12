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
// IsUniversalCell reports whether the (client, upstream) pair is one of the
// three cross-wire cells the universal matrix ADDS: chat→Messages,
// Responses→chat, Responses→Messages. The other three cross-wire pairs keep
// their characterized legacy converters; these three ride the strict shared
// request/response/event codecs. The pair alone decides, so the router's
// selection and the proxy's codec choice cannot disagree.
func IsUniversalCell(client, upstream provider.Surface) bool {
	if client == upstream {
		return false
	}
	switch client {
	case provider.SurfaceOpenAI:
		return upstream == provider.SurfaceAnthropic
	case provider.SurfaceResponses:
		return upstream == provider.SurfaceOpenAI || upstream == provider.SurfaceAnthropic
	}
	return false
}

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
