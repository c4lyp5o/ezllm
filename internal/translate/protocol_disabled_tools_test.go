package translate

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestProtocolDisabledTools(t *testing.T) {
	for _, stream := range []bool{false, true} {
		b, err := AnthropicToResponses([]byte(fmt.Sprintf(`{"max_tokens":17,"stream":%t,"messages":[{"role":"user","content":"hello"}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}],"tool_choice":{"type":"none"}}`, stream)), "model")
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			Choice string `json:"tool_choice"`
		}
		if err = json.Unmarshal(b, &out); err != nil {
			t.Fatal(err)
		}
		if out.Choice != "none" {
			t.Fatalf("disabled tools reenabled: %s", b)
		}
	}
}
