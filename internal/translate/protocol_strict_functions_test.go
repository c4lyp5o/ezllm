package translate

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

func TestProtocolStrictFunctions(t *testing.T) {
	for _, value := range []string{"", `,"strict":true`, `,"strict":false`} {
		for _, source := range []provider.Surface{provider.SurfaceResponses, provider.SurfaceOpenAI, provider.SurfaceAnthropic} {
			var body string
			if source == provider.SurfaceResponses {
				body = fmt.Sprintf(`{"input":"hello","tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{},"additionalProperties":false}%s}]}`, value)
			} else if source == provider.SurfaceAnthropic {
				body = fmt.Sprintf(`{"max_tokens":17,"messages":[{"role":"user","content":"hello"}],"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{},"additionalProperties":false}%s}]}`, value)
			} else {
				body = fmt.Sprintf(`{"messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{},"additionalProperties":false}%s}}]}`, value)
			}
			for _, target := range []provider.Surface{provider.SurfaceOpenAI, provider.SurfaceAnthropic, provider.SurfaceResponses} {
				var b []byte
				var err error
				switch {
				case source == provider.SurfaceOpenAI && target == provider.SurfaceResponses:
					b, err = ChatToResponses([]byte(body), "model")
				case source == provider.SurfaceAnthropic && target == provider.SurfaceResponses:
					b, err = AnthropicToResponses([]byte(body), "model")
				case source == provider.SurfaceAnthropic && target == provider.SurfaceOpenAI:
					b, err = Request([]byte(body), "model")
				default:
					b, err = RequestUniversal([]byte(body), source, target, "model")
				}
				if err != nil {
					t.Fatal(err)
				}
				var out struct {
					Tools []map[string]json.RawMessage `json:"tools"`
				}
				if err = json.Unmarshal(b, &out); err != nil {
					t.Fatal(err)
				}
				tool := out.Tools[0]
				if target == provider.SurfaceOpenAI {
					raw := tool["function"]
					tool = nil
					if err = json.Unmarshal(raw, &tool); err != nil {
						t.Fatal(err)
					}
				}
				want := strings.TrimPrefix(value, `,"strict":`)
				if string(tool["strict"]) != want {
					t.Fatalf("%s -> %s strict=%q want %q: %s", source, target, tool["strict"], want, b)
				}
			}
		}
	}
}
