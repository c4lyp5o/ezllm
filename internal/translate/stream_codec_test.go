package translate

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

func codecStream(t *testing.T, input string, from, to provider.Surface, opts CodecOptions) (*StreamReader, []byte) {
	t.Helper()
	r := NewStreamReader(strings.NewReader(input), from, to, "client-alias", opts)
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return r, b
}

const portableChatStream = "data: {\"id\":\"chat_x\",\"created\":123,\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello 😊\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"chat_x\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_A\",\"function\":{\"name\":\"ls\",\"arguments\":\"{\\\"path\\\":\"}}]},\"finish_reason\":null}]}\n\n" +
	"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"src\\\"}\"}}]},\"finish_reason\":null}]}\n\n" +
	"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":7}}\n\ndata: [DONE]\n\n"

// A Responses source quotes argument fragments by item id in some frames and
// by call id in others, and the client-visible tool id is the call id: losing
// either identity silently drops argument text or leaks the upstream item id.
func TestStreamPortableToolIdentityByCallID(t *testing.T) {
	in := `event: response.created
data: {"type":"response.created","response":{"id":"resp_i"}}

event: response.output_item.added
data: {"type":"response.output_item.added","item":{"id":"fc_x","type":"function_call","call_id":"call_A","name":"ls"}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","item_id":"fc_x","delta":"{\"path\":"}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","call_id":"call_A","delta":"\"src\"}"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_i","status":"completed","output":[{"id":"fc_x","type":"function_call","call_id":"call_A","name":"ls","arguments":"{\"path\":\"src\"}"}],"usage":{"input_tokens":3,"output_tokens":4}}}

`
	for _, to := range []provider.Surface{provider.SurfaceChat, provider.SurfaceAnthropic, provider.SurfaceResponses} {
		r, b := codecStream(t, in, provider.SurfaceResponses, to, CodecOptions{})
		if r.Outcome() != FinishTools {
			t.Fatalf("%s outcome %s: %s", to, r.Outcome(), b)
		}
		if !bytes.Contains(b, []byte(`{\"path\":`)) || !bytes.Contains(b, []byte(`\"src\"}`)) {
			t.Fatalf("%s dropped an argument fragment quoted by the other identity: %s", to, b)
		}
		if !bytes.Contains(b, []byte("call_A")) {
			t.Fatalf("%s lost the client-visible call id: %s", to, b)
		}
		if bytes.Contains(b, []byte("fc_x")) {
			t.Fatalf("%s leaked the upstream item id as the tool id: %s", to, b)
		}
	}
}

func TestStreamPortableResponsesLifecycle(t *testing.T) {
	r, b := codecStream(t, portableChatStream, provider.SurfaceChat, provider.SurfaceResponses, CodecOptions{})
	if r.Outcome() != FinishTools {
		t.Fatalf("outcome %s: %s", r.Outcome(), b)
	}
	frames := parseFrames(t, b)
	var terminal map[string]any
	seq := -1
	added := map[string]int{}
	for _, f := range frames {
		var p map[string]any
		if err := json.Unmarshal([]byte(f.Data), &p); err != nil {
			t.Fatal(err)
		}
		if p["type"] != f.Event {
			t.Errorf("event/type differ %s: %s", f.Event, f.Data)
		}
		n, ok := p["sequence_number"].(float64)
		if !ok || int(n) != seq+1 {
			t.Fatalf("sequence discontinuity %s", f.Data)
		}
		seq = int(n)
		if f.Event == "response.output_item.added" {
			it := p["item"].(map[string]any)
			added[it["id"].(string)] = int(p["output_index"].(float64))
		}
		if strings.HasSuffix(f.Event, ".delta") {
			id, _ := p["item_id"].(string)
			if idx, ok := added[id]; !ok || idx != int(p["output_index"].(float64)) {
				t.Fatalf("delta identity mismatch %s", f.Data)
			}
			if f.Event == "response.output_text.delta" {
				if _, ok := p["content_index"]; !ok {
					t.Fatal("missing content_index")
				}
				if _, ok := p["logprobs"]; !ok {
					t.Fatal("missing logprobs")
				}
			}
		}
		if f.Event == "response.completed" {
			terminal = p["response"].(map[string]any)
		}
	}
	if terminal == nil || frames[0].Event != "response.created" || frames[1].Event != "response.in_progress" {
		t.Fatalf("lifecycle missing: %s", b)
	}
	out := terminal["output"].([]any)
	if len(out) != 2 {
		t.Fatalf("output %v", out)
	}
	call := out[1].(map[string]any)
	if call["call_id"] != "call_A" || call["arguments"] != `{"path":"src"}` || call["name"] != "ls" || call["id"] == call["call_id"] {
		t.Fatalf("call identity/args %v", call)
	}
	u := terminal["usage"].(map[string]any)
	if u["input_tokens"] != float64(12) || u["output_tokens"] != float64(7) || u["total_tokens"] != float64(19) {
		t.Fatalf("usage %v", u)
	}
	// Decode the emitted wire back into event IR: no fixture echo can pass this.
	round, chat := codecStream(t, string(b), provider.SurfaceResponses, provider.SurfaceChat, CodecOptions{})
	if round.Outcome() != FinishTools || !bytes.Contains(chat, []byte("hello 😊")) || !bytes.Contains(chat, []byte(`"id":"call_A"`)) {
		t.Fatalf("roundtrip %s", chat)
	}
}

func TestStreamPortableTerminalAndAbsentUsage(t *testing.T) {
	for _, to := range []provider.Surface{provider.SurfaceChat, provider.SurfaceResponses, provider.SurfaceAnthropic} {
		t.Run(string(to), func(t *testing.T) {
			// A finish chunk is a terminal classification, usage may arrive later.
			input := `data: {"id":"c","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"length"}]}` + "\n\ndata: [DONE]\n\n"
			r, b := codecStream(t, input, provider.SurfaceChat, to, CodecOptions{})
			if r.Outcome() != FinishLength {
				t.Fatalf("outcome %s: %s", r.Outcome(), b)
			}
			if bytes.Contains(b, []byte(`"input_tokens":0`)) || bytes.Contains(b, []byte(`"prompt_tokens":0`)) {
				t.Fatalf("fabricated usage %s", b)
			}
		})
	}
}

func TestStreamPortableTruncationAndLateError(t *testing.T) {
	for _, to := range []provider.Surface{provider.SurfaceAnthropic, provider.SurfaceChat, provider.SurfaceResponses} {
		for _, tail := range []string{"", "data: {\"error\":{\"type\":\"server_error\",\"message\":\"late boom\"}}\n\n", "data: not-json\n\n"} {
			input := "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n" + tail
			r, b := codecStream(t, input, provider.SurfaceChat, to, CodecOptions{})
			if r.Outcome() != FinishFailed || !bytes.Contains(b, []byte("partial")) || !bytes.Contains(b, []byte(`"error"`)) {
				t.Fatalf("failed stream became success %s %s", to, b)
			}
			if bytes.Contains(b, []byte("response.completed")) || bytes.Contains(b, []byte("message_stop")) {
				t.Fatalf("success terminal after error %s", b)
			}
		}
	}
}

func TestStreamPortableMessagesUsageMerge(t *testing.T) {
	in := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_source\",\"usage\":{\"input_tokens\":3,\"cache_read_input_tokens\":5,\"cache_creation_input_tokens\":2}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":8}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	for _, to := range []provider.Surface{provider.SurfaceChat, provider.SurfaceResponses} {
		r, b := codecStream(t, in, provider.SurfaceAnthropic, to, CodecOptions{MessagesUsage: UsageExclusive})
		if r.Outcome() != FinishCompleted {
			t.Fatal(string(b))
		}
		key := `"prompt_tokens":10`
		if to == provider.SurfaceResponses {
			key = `"input_tokens":10`
		}
		if !bytes.Contains(b, []byte(key)) || !bytes.Contains(b, []byte(`"output_tokens":8`)) && !bytes.Contains(b, []byte(`"completion_tokens":8`)) {
			t.Fatalf("merge %s", b)
		}
	}
	r, b := codecStream(t, in, provider.SurfaceAnthropic, provider.SurfaceChat, CodecOptions{})
	if r.Outcome() != FinishFailed {
		t.Fatalf("unknown convention guessed: %s", b)
	}
}

func TestStreamPortableSSEFraming(t *testing.T) {
	input := ": comment\r\nevent: ignored\r\ndata:{\"id\":\"c\",\r\ndata: \"choices\":[{\"index\":0,\"delta\":{\"content\":\"é😊\"},\"finish_reason\":\"stop\"}]}\r\n\r\ndata:[DONE]\r\n\r\n"
	_, out := codecStream(t, input, provider.SurfaceChat, provider.SurfaceResponses, CodecOptions{})
	if !bytes.Contains(out, []byte("é😊")) || !bytes.Contains(out, []byte("response.completed")) {
		t.Fatalf("framing %s", out)
	}
	// Transport reads can split inside any UTF-8 sequence, but JSON is decoded
	// only after a complete frame, never per network fragment.
	r := NewStreamReader(&oneByteReader{data: []byte(input)}, provider.SurfaceChat, provider.SurfaceResponses, "client", CodecOptions{})
	b, err := io.ReadAll(r)
	if err != nil || !bytes.Contains(b, []byte("é😊")) {
		t.Fatalf("fragmented stream %v %s", err, b)
	}
}

type oneByteReader struct{ data []byte }

func (r *oneByteReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	p[0] = r.data[0]
	r.data = r.data[1:]
	return 1, nil
}

func TestStreamPortableCloseAndFlowControl(t *testing.T) {
	pr, pw := io.Pipe()
	r := NewStreamReader(pr, provider.SurfaceChat, provider.SurfaceResponses, "client", CodecOptions{})
	defer r.Close()
	defer pw.Close()
	// Source remains parked WITHOUT sending finish. The client must get delta.
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		_, _ = io.WriteString(pw, "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"early\"},\"finish_reason\":null}]}\n\n")
	}()
	got := make(chan string, 1)
	go func() {
		var b bytes.Buffer
		buf := make([]byte, 128)
		for {
			n, e := r.Read(buf)
			b.Write(buf[:n])
			if strings.Contains(b.String(), `"delta":"early"`) || e != nil {
				got <- b.String()
				return
			}
		}
	}()
	select {
	case b := <-got:
		if !strings.Contains(b, `"delta":"early"`) {
			t.Fatal(b)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("client delta blocked waiting for terminal")
	}
	<-sent
	// Closing the codec forwards Close to the source and unblocks a blocked Read.
	closed := make(chan error, 1)
	go func() { _, e := r.Read(make([]byte, 1024)); closed <- e }()
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not unblock source read")
	}
}

func TestStreamPortableInvalidArgumentsAndCap(t *testing.T) {
	bad := strings.ReplaceAll(portableChatStream, `{\"path\":`, `[`)
	r, b := codecStream(t, bad, provider.SurfaceChat, provider.SurfaceAnthropic, CodecOptions{})
	if r.Outcome() != FinishFailed || !bytes.Contains(b, []byte(`"error"`)) {
		t.Fatalf("invalid object succeeded: %s", b)
	}
	r = NewStreamReader(strings.NewReader(portableChatStream), provider.SurfaceChat, provider.SurfaceResponses, "client", CodecOptions{})
	r.maxOutput = 4 // exercise the same 32MiB limit without wasting memory in test
	b, e := io.ReadAll(r)
	if e != nil {
		t.Fatal(e)
	}
	if r.Outcome() != FinishFailed || !bytes.Contains(b, []byte("output limit")) {
		t.Fatalf("cap %s", b)
	}
}

func TestStreamPortableSourceFinishFailureUsage(t *testing.T) {
	for _, kind := range []string{"incomplete", "failed"} {
		in := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_c\"}}\n\n" +
			"event: response." + kind + "\ndata: {\"type\":\"response." + kind + "\",\"response\":{\"id\":\"resp_c\",\"output\":[],\"status\":\"" + kind + "\",\"error\":{\"message\":\"boom\",\"type\":\"api_error\"},\"usage\":{\"input_tokens\":4,\"output_tokens\":6}}}\n\n"
		r, b := codecStream(t, in, provider.SurfaceResponses, provider.SurfaceChat, CodecOptions{})
		want := FinishLength
		if kind == "failed" {
			want = FinishFailed
		}
		if r.Outcome() != want || !bytes.Contains(b, []byte(`"prompt_tokens":4`)) || !bytes.Contains(b, []byte(`"completion_tokens":6`)) {
			t.Fatalf("terminal usage %s", b)
		}
	}
}
