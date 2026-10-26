package translate

import (
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

func boolPtr(b bool) *bool { return &b }

func TestEligible(t *testing.T) {
	cases := []struct {
		name           string
		surface        provider.Surface
		protoAnthropic *bool
		protoOpenAI    *bool
		enabled        bool
		want           bool
	}{
		// The one true case: anthropic client, proven openai-only hop, on.
		{"proven openai-only hop serves anthropic client",
			provider.SurfaceAnthropic, boolPtr(false), boolPtr(true), true, true},
		// Native path merely untested does not disqualify.
		{"proto_anthropic NULL still eligible",
			provider.SurfaceAnthropic, nil, boolPtr(true), true, true},

		{"disabled knob", provider.SurfaceAnthropic, boolPtr(false), boolPtr(true), false, false},
		{"openai client never translated", provider.SurfaceChat, boolPtr(false), boolPtr(true), true, false},
		{"responses client never translated", provider.SurfaceResponses, boolPtr(false), boolPtr(true), true, false},
		{"openai capability untested", provider.SurfaceAnthropic, boolPtr(false), nil, true, false},
		{"openai capability proven false", provider.SurfaceAnthropic, boolPtr(false), boolPtr(false), true, false},
		{"native-capable hop stays native", provider.SurfaceAnthropic, boolPtr(true), boolPtr(true), true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Eligible(tc.surface, tc.protoAnthropic, tc.protoOpenAI, tc.enabled); got != tc.want {
				t.Errorf("Eligible(%s, protoAnthropic=%v, protoOpenAI=%v, enabled=%v) = %v, want %v",
					tc.surface, tc.protoAnthropic, tc.protoOpenAI, tc.enabled, got, tc.want)
			}
		})
	}
}
