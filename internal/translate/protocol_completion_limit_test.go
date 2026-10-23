package translate

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestProtocolCompletionLimit(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, limits := range []string{`"max_completion_tokens":17`, `"max_tokens":17`, `"max_tokens":17,"max_completion_tokens":17`} {
			b, err := ChatToResponses([]byte(fmt.Sprintf(`{"messages":[{"role":"user","content":"hello"}],"stream":%t,%s}`, stream, limits)), "model")
			if err != nil {
				t.Fatal(err)
			}
			var out struct {
				Max *int `json:"max_output_tokens"`
			}
			if err = json.Unmarshal(b, &out); err != nil {
				t.Fatal(err)
			}
			if out.Max == nil || *out.Max != 17 {
				t.Fatalf("limit lost: %s", b)
			}
		}
	}
	if _, err := ChatToResponses([]byte(`{"max_tokens":17,"max_completion_tokens":18}`), "model"); err == nil {
		t.Fatal("conflicting limits accepted")
	}
	b, err := ChatToResponses([]byte(`{"messages":[]}`), "model")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"max_output_tokens"`) {
		t.Fatalf("invented limit: %s", b)
	}
}
