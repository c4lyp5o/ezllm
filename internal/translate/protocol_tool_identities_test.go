package translate

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

func TestProtocolToolIdentities(t *testing.T) {
	sum := sha256.Sum256([]byte("call.a"))
	ids := []string{"call.a", "call_a", "call/a", "functions.read:0", "functions_read_0", fmt.Sprintf("call_a_%x", sum[:8])}
	for _, source := range []provider.Surface{provider.SurfaceChat, provider.SurfaceResponses} {
		ir := &RequestIR{}
		calls := Turn{Role: RoleAssistant}
		for i, id := range ids {
			calls.Blocks = append(calls.Blocks, Block{Kind: BlockCall, CallID: id, Name: fmt.Sprintf("f%d", i), Arguments: "{}"})
		}
		ir.Turns = append(ir.Turns, calls)
		for i, id := range ids {
			ir.Turns = append(ir.Turns, Turn{Role: RoleTool, Blocks: []Block{{Kind: BlockResult, CallID: id, Text: fmt.Sprintf("result%d", i)}}})
		}
		body, err := encodeRequestFor(ir, source, "model")
		if err != nil {
			t.Fatal(err)
		}
		b, err := RequestUniversal(body, source, provider.SurfaceAnthropic, "model")
		if err != nil {
			t.Fatal(err)
		}
		again, err := RequestUniversal(body, source, provider.SurfaceAnthropic, "model")
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != string(again) {
			t.Fatal("unstable mapping")
		}
		var out struct {
			Messages []struct {
				Content []struct {
					Type    string `json:"type"`
					ID      string `json:"id"`
					Use     string `json:"tool_use_id"`
					Name    string `json:"name"`
					Content string `json:"content"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err = json.Unmarshal(b, &out); err != nil {
			t.Fatal(err)
		}
		seen := map[string]string{}
		index := 0
		for _, m := range out.Messages {
			for _, v := range m.Content {
				if v.Type == "tool_use" {
					if _, ok := seen[v.ID]; ok {
						t.Fatalf("duplicate identity: %s", b)
					}
					if messagesToolID(v.ID) != v.ID {
						t.Fatalf("illegal id %q", v.ID)
					}
					seen[v.ID] = v.Name
					for i, original := range ids {
						if messagesToolID(original) == original && v.Name == fmt.Sprintf("f%d", i) && v.ID != original {
							t.Fatalf("legal identity changed: %s", b)
						}
					}
				}
				if v.Type == "tool_result" {
					if seen[v.Use] != fmt.Sprintf("f%d", index) || v.Content != fmt.Sprintf("result%d", index) {
						t.Fatalf("result mispaired: %s", b)
					}
					index++
				}
			}
		}
		if len(seen) != len(ids) || index != len(ids) {
			t.Fatalf("lost history: %s", b)
		}
	}
}
