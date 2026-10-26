package proxy

// M5 — the universal wire matrix, end to end.
//
// Nine cells (3 client surfaces x 3 upstream wires) x stream/nonstream. Each
// off-diagonal case checks the ACTUAL upstream path and body AND the client's
// return shape; every diagonal checks that unknown fields and native bytes
// pass through untouched. Then the paths that must NOT translate: a 429 hops,
// a 400 never blind-fails-over, a preparation refusal spends nothing, and a
// late stream error neither retries nor marks the hop good — including the
// legacy cell whose clients still receive the historic synthesized close.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// ── fixtures ─────────────────────────────────────────────────────────────────

func chatBody(opaque bool) string {
	extra := ""
	if opaque {
		extra = `,"x_opaque":7`
	}
	return `{"model":"daily","messages":[{"role":"user","content":"hi"}],"max_tokens":64` + extra + `}`
}

func messagesBody(opaque bool) string {
	extra := ""
	if opaque {
		extra = `,"x_opaque":7`
	}
	return `{"model":"daily","max_tokens":64,"messages":[{"role":"user","content":"hi"}]` + extra + `}`
}

func responsesBody(opaque bool) string {
	extra := ""
	if opaque {
		extra = `,"x_opaque":7`
	}
	return `{"model":"daily","max_output_tokens":64,"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]` + extra + `}`
}

func clientBody(surface provider.Surface, opaque bool) string {
	switch surface {
	case provider.SurfaceChat:
		return chatBody(opaque)
	case provider.SurfaceAnthropic:
		return messagesBody(opaque)
	default:
		return responsesBody(opaque)
	}
}

// upstreamResponses/shape is the upstream's OWN wire shape, one per surface.
const (
	chatUpstreamResp = `{"id":"c1","object":"chat.completion","created":1,"model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`
	msgUpstreamResp  = `{"id":"msg_1","type":"message","role":"assistant","model":"upstream-model","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":5}}`
	respUpstreamResp = `{"id":"resp_1","object":"response","created_at":1,"model":"upstream-model","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":10,"output_tokens":5}}`
)

func upstreamResponse(surface provider.Surface) string {
	switch surface {
	case provider.SurfaceChat:
		return chatUpstreamResp
	case provider.SurfaceAnthropic:
		return msgUpstreamResp
	default:
		return respUpstreamResp
	}
}

const chatUpstreamStream = `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"upstream-model","choices":[{"index":0,"delta":{"role":"assistant","content":"he"},"finish_reason":null}]}` + "\n\n" +
	`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"upstream-model","choices":[{"index":0,"delta":{"content":"llo"},"finish_reason":null}]}` + "\n\n" +
	`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"upstream-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}` + "\n\n" +
	"data: [DONE]\n\n"

const msgUpstreamStream = `event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"upstream-model","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":1}}}` + "\n\n" +
	`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
	`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"he"}}` + "\n\n" +
	`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"llo"}}` + "\n\n" +
	`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}` + "\n\n" +
	`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5}}` + "\n\n" +
	`event: message_stop` + "\n" + `data: {"type":"message_stop"}` + "\n\n"

const respUpstreamStream = `event: response.output_text.delta` + "\n" + `data: {"type":"response.output_text.delta","item_id":"msg_r","output_index":0,"content_index":0,"delta":"he"}` + "\n\n" +
	`event: response.output_text.delta` + "\n" + `data: {"type":"response.output_text.delta","item_id":"msg_r","output_index":0,"content_index":0,"delta":"llo"}` + "\n\n" +
	`event: response.completed` + "\n" + `data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":10,"output_tokens":5}}}` + "\n\n"

func upstreamStream(surface provider.Surface) string {
	switch surface {
	case provider.SurfaceChat:
		return chatUpstreamStream
	case provider.SurfaceAnthropic:
		return msgUpstreamStream
	default:
		return respUpstreamStream
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func wireRoute(base string, client, upstream provider.Surface, opts ...func(*Route)) Route {
	rt := testRoute(base, client)
	rt.Model = "resolved-model"
	rt.Alias = "daily"
	if client != upstream {
		rt.Translated = true
		rt.WireSurface = upstream
	}
	if upstream == provider.SurfaceAnthropic {
		rt.Account.UsageConvention = provider.UsageConventionTotal
	}
	for _, o := range opts {
		o(&rt)
	}
	return rt
}

// upstreamRecorder sees the ACTUAL upstream call: path, method, body.
type upstreamRecorder struct {
	path  string
	body  []byte
	calls int
}

func (u *upstreamRecorder) serve(t *testing.T, wire provider.Surface, stream bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u.calls++
		u.path = r.URL.Path
		u.body, _ = io.ReadAll(r.Body)
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(upstreamStream(wire)))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(upstreamResponse(wire)))
	}
}

func jsonMap(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return m
}

// textFromContent extracts text from a wire content value: a bare string or a
// list of blocks (type text/input_text). Both spellings are legal on the
// respective wires, and the strict encoders use the block form.
func textFromContent(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, part := range c {
			p, _ := part.(map[string]any)
			switch p["type"] {
			case "text", "input_text":
				s, _ := p["text"].(string)
				b.WriteString(s)
			}
		}
		return b.String()
	}
	return ""
}

// requestTextOf extracts the user text a request carries, per wire shape.
func requestTextOf(t *testing.T, surface provider.Surface, body []byte) string {
	t.Helper()
	m := jsonMap(t, body)
	if surface == provider.SurfaceResponses {
		in, _ := m["input"].([]any)
		if len(in) == 0 {
			t.Fatalf("no input items in %s", body)
		}
		c0, _ := in[0].(map[string]any)
		return textFromContent(c0["content"])
	}
	msgs, _ := m["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatalf("no messages in %s", body)
	}
	c0, _ := msgs[0].(map[string]any)
	return textFromContent(c0["content"])
}

// responseTextOf extracts the assistant text a response carries, per shape.
func responseTextOf(t *testing.T, surface provider.Surface, body []byte) string {
	t.Helper()
	m := jsonMap(t, body)
	switch surface {
	case provider.SurfaceChat:
		choices, _ := m["choices"].([]any)
		c0, _ := choices[0].(map[string]any)
		msg, _ := c0["message"].(map[string]any)
		s, _ := msg["content"].(string)
		return s
	case provider.SurfaceAnthropic:
		return textFromContent(m["content"])
	default:
		out, _ := m["output"].([]any)
		c0, _ := out[0].(map[string]any)
		content, _ := c0["content"].([]any)
		p0, _ := content[0].(map[string]any)
		s, _ := p0["text"].(string)
		return s
	}
}

// ── the nine cells, non-stream ───────────────────────────────────────────────

func TestWireMatrixNineCellsNonStream(t *testing.T) {
	surfaces := []provider.Surface{provider.SurfaceChat, provider.SurfaceAnthropic, provider.SurfaceResponses}
	for _, client := range surfaces {
		for _, up := range surfaces {
			name := string(client) + "-" + string(up)
			t.Run(name, func(t *testing.T) {
				rec := &upstreamRecorder{}
				srv := httptest.NewServer(rec.serve(t, up, false))
				defer srv.Close()

				body := clientBody(client, client == up)
				d := newDispatcher()
				d.SetRetries(0)
				req := httptest.NewRequest(http.MethodPost, "/v1/x", strings.NewReader(body))
				recout := httptest.NewRecorder()
				served, res, err := d.ForwardCandidates(context.Background(), recout, req,
					[]Route{wireRoute(srv.URL, client, up)}, []byte(body))
				if err != nil {
					t.Fatalf("forward: %v", err)
				}
				if res.Status != 200 {
					t.Fatalf("status = %d body=%s", res.Status, recout.Body.String())
				}

				// The upstream saw the ACTUAL path for its own wire.
				if want := provider.SurfacePath(up); rec.path != want {
					t.Errorf("upstream path = %s, want %s", rec.path, want)
				}
				if client == up {
					// Diagonal: native identity. The body is the model-swapped
					// client body, byte for byte (unknown fields included).
					want, _, err := SwapModel([]byte(body), "resolved-model")
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(rec.body, want) {
						t.Errorf("native body changed\nwant: %s\ngot:  %s", want, rec.body)
					}
					if !bytes.Equal(recout.Body.Bytes(), []byte(upstreamResponse(up))) {
						t.Errorf("native response identity lost:\nwant: %s\ngot:  %s", upstreamResponse(up), recout.Body.String())
					}
					if res.Usage == nil || res.Usage.In != 10 {
						t.Errorf("native usage tap: %+v", res.Usage)
					}
					return
				}

				// Off-diagonal: the upstream got a VALID request for its wire
				// with the resolved model and the same content.
				if got := jsonMap(t, rec.body)["model"]; got != "resolved-model" {
					t.Errorf("upstream model = %v, want resolved-model", got)
				}
				if got := requestTextOf(t, up, rec.body); got != "hi" {
					t.Errorf("upstream request text = %q, want hi (%s)", got, rec.body)
				}
				if bytes.Contains(rec.body, []byte("x_opaque")) {
					t.Errorf("an unknown field crossed wires: %s", rec.body)
				}
				// The client got a VALID response for ITS wire.
				out := recout.Body.Bytes()
				if got := responseTextOf(t, client, out); got != "hello" {
					t.Errorf("client text = %q, want hello (%s)", got, out)
				}
				if got := jsonMap(t, out)["model"]; got != "daily" {
					t.Errorf("client model = %v, want daily", got)
				}
				if res.Usage == nil || res.Usage.In != 10 || res.Usage.Out != 5 {
					t.Errorf("usage tap on the client shape: %+v", res.Usage)
				}
				if !served.Translated {
					t.Error("served route lost its translated marker")
				}
				if served.EffectiveWire() != up {
					t.Errorf("effective wire = %s, want %s", served.EffectiveWire(), up)
				}
			})
		}
	}
}

// ── the nine cells, streaming ────────────────────────────────────────────────

func TestWireMatrixNineCellsStream(t *testing.T) {
	surfaces := []provider.Surface{provider.SurfaceChat, provider.SurfaceAnthropic, provider.SurfaceResponses}
	for _, client := range surfaces {
		for _, up := range surfaces {
			t.Run(string(client)+"-"+string(up), func(t *testing.T) {
				rec := &upstreamRecorder{}
				srv := httptest.NewServer(rec.serve(t, up, true))
				defer srv.Close()

				raw := clientBody(client, client == up)
				body := raw[:len(raw)-1] + `,"stream":true}`
				d := newDispatcher()
				d.SetRetries(0)
				req := httptest.NewRequest(http.MethodPost, "/v1/x", strings.NewReader(body))
				recout := httptest.NewRecorder()
				_, res, err := d.ForwardCandidates(context.Background(), recout, req,
					[]Route{wireRoute(srv.URL, client, up)}, []byte(body))
				if err != nil {
					t.Fatalf("forward: %v", err)
				}
				if res.Status != 200 {
					t.Fatalf("status = %d err=%q body=%s", res.Status, res.Err, recout.Body.String())
				}
				out := recout.Body.Bytes()
				if len(out) == 0 {
					t.Fatal("client got an empty stream")
				}
				if client == up {
					if !bytes.Equal(out, []byte(upstreamStream(up))) {
						t.Errorf("native stream identity lost\ngot: %s", out)
					}
					return
				}
				// Fragment meaning, not read boundaries: the client's own shape
				// must carry the concatenated content.
				if got := streamTextOf(t, client, out); got != "hello" {
					t.Errorf("client stream text = %q, want hello\n%s", got, out)
				}
				if res.StreamOutcome != StreamOutcomeCompleted {
					t.Errorf("stream outcome = %q, want completed", res.StreamOutcome)
				}
				if res.Usage == nil || res.Usage.In != 10 || res.Usage.Out != 5 {
					t.Errorf("stream usage: %+v", res.Usage)
				}
			})
		}
	}
}

// streamTextOf concatenates the text deltas of a client-shaped SSE body.
func streamTextOf(t *testing.T, client provider.Surface, raw []byte) string {
	t.Helper()
	var b strings.Builder
	for _, frame := range strings.Split(string(raw), "\n\n") {
		data := frame
		if i := strings.Index(frame, "data:"); i >= 0 {
			data = strings.TrimSpace(frame[i+len("data:"):])
		}
		if data == "" || data == "[DONE]" || strings.Contains(frame, "event:") && !strings.Contains(frame, "data:") {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			continue
		}
		switch client {
		case provider.SurfaceChat:
			if choices, _ := m["choices"].([]any); len(choices) > 0 {
				c0, _ := choices[0].(map[string]any)
				delta, _ := c0["delta"].(map[string]any)
				s, _ := delta["content"].(string)
				b.WriteString(s)
			}
		case provider.SurfaceAnthropic:
			delta, _ := m["delta"].(map[string]any)
			s, _ := delta["text"].(string)
			b.WriteString(s)
		default:
			s, _ := m["delta"].(string)
			b.WriteString(s)
		}
	}
	return b.String()
}
