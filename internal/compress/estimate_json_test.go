package compress

import (
	"encoding/json"
	"testing"
)

// estimateJSON must equal the marshal-and-count estimate it replaces.
func TestEstimateJSONMatchesMarshalEstimate(t *testing.T) {
	for _, v := range []any{
		nil, "", "a", "héllo wörld 日本語", "<tag>&amp;", "bad \xff utf8", json.Number("12.50"),
		[]any{map[string]any{"role": "user", "content": "hi"}, map[string]any{"role": "tool", "content": "x\ny\tz"}},
		map[string]any{"k": []any{1.5, true, nil, " "}},
	} {
		if got, want := estimateJSON(v), EstimateTokens(string(mustMarshal(v))); got != want {
			t.Errorf("%#v: estimateJSON = %d, marshal estimate = %d", v, got, want)
		}
	}
	msgs := decodeMsgs(t, benchConversation(5))
	if got, want := estimateJSON(msgs), EstimateTokens(string(mustMarshal(msgs))); got != want {
		t.Errorf("conversation: estimateJSON = %d, want %d", got, want)
	}
}
