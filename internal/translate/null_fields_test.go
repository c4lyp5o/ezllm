package translate

import "testing"

// SDKs send unset options as null. They are left out, not read as 0 or false,
// and tool_choice: null no longer fails the request. Every client wire.
func TestNullFieldsAreAbsent(t *testing.T) {
	for name, body := range map[string]string{
		"chat":      `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":null,"temperature":null,"top_p":null,"parallel_tool_calls":null,"stop":null,"tool_choice":null,"tools":null}`,
		"messages":  `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"temperature":null,"top_p":null,"stop_sequences":null,"tool_choice":null,"tools":null}`,
		"responses": `{"model":"m","input":"hi","max_output_tokens":null,"temperature":null,"top_p":null,"parallel_tool_calls":null,"tool_choice":null,"tools":null}`,
	} {
		var ir *RequestIR
		var err error
		switch name {
		case "chat":
			ir, err = DecodeChatRequest([]byte(body))
		case "messages":
			ir, err = DecodeMessagesRequest([]byte(body))
		case "responses":
			ir, err = DecodeResponsesRequest([]byte(body))
		}
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if name != "messages" && ir.MaxOutputTokens != nil {
			t.Errorf("%s: max tokens = %d, want absent", name, *ir.MaxOutputTokens)
		}
		if ir.Temperature != nil || ir.TopP != nil || ir.ParallelToolCalls != nil || len(ir.Stop) != 0 {
			t.Errorf("%s: a null field became a value", name)
		}
	}
}
