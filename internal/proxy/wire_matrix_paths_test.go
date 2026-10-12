package proxy

// M5 — the matrix paths that must NOT translate into surprises: a tool call
// replays and returns with its public identity through every cross-wire cell,
// a 429 hops across wires, a 400 never blind-fails-over, a preparation
// refusal spends nothing, and a late stream error neither retries nor marks
// the hop good — even where a legacy client still sees the historic
// synthesized close.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/translate"
)

// crossCells are the six translated cells, in their (client, upstream) pair.
func crossCells() [][2]provider.Surface {
	return [][2]provider.Surface{
		{provider.SurfaceAnthropic, provider.SurfaceOpenAI},
		{provider.SurfaceAnthropic, provider.SurfaceResponses},
		{provider.SurfaceOpenAI, provider.SurfaceAnthropic},
		{provider.SurfaceOpenAI, provider.SurfaceResponses},
		{provider.SurfaceResponses, provider.SurfaceOpenAI},
		{provider.SurfaceResponses, provider.SurfaceAnthropic},
	}
}

// toolHistory is a request whose history contains a prior assistant tool call
// and its result, in the CLIENT's shape.
func toolHistory(client provider.Surface) string {
	switch client {
	case provider.SurfaceOpenAI:
		return `{"model":"daily","max_tokens":64,"messages":[` +
			`{"role":"user","content":"list files"},` +
			`{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"ls","arguments":"{}"}}]},` +
			`{"role":"tool","tool_call_id":"call_1","content":"a.txt"}]}`
	case provider.SurfaceAnthropic:
		return `{"model":"daily","max_tokens":64,"messages":[` +
			`{"role":"user","content":"list files"},` +
			`{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"ls","input":{}}]},` +
			`{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"a.txt"}]}]}`
	default:
		return `{"model":"daily","max_output_tokens":64,"input":[` +
			`{"role":"user","content":[{"type":"input_text","text":"list files"}]},` +
			`{"type":"function_call","call_id":"call_1","name":"ls","arguments":"{}"},` +
			`{"type":"function_call_output","call_id":"call_1","output":"a.txt"}]}`
	}
}

// toolCallResponse is an upstream answer that asks for ONE tool call, in the
// UPSTREAM's shape.
func toolCallResponse(upstream provider.Surface) string {
	switch upstream {
	case provider.SurfaceOpenAI:
		return `{"id":"c2","object":"chat.completion","created":1,"model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call_2","type":"function","function":{"name":"ls","arguments":"{\"path\":\".\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`
	case provider.SurfaceAnthropic:
		return `{"id":"msg_2","type":"message","role":"assistant","model":"upstream-model","content":[{"type":"tool_use","id":"call_2","name":"ls","input":{"path":"."}}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":5}}`
	default:
		return `{"id":"resp_2","object":"response","created_at":1,"model":"upstream-model","status":"completed","output":[{"type":"function_call","id":"fc_2","call_id":"call_2","name":"ls","arguments":"{\"path\":\".\"}"}],"usage":{"input_tokens":10,"output_tokens":5}}`
	}
}

// clientToolCallOf asserts the client's shape carries the tool call with its
// PUBLIC identity (call_2), whatever the upstream called the item.
func clientToolCallOf(t *testing.T, client provider.Surface, body []byte) (string, string) {
	t.Helper()
	m := jsonMap(t, body)
	switch client {
	case provider.SurfaceOpenAI:
		choices, _ := m["choices"].([]any)
		c0, _ := choices[0].(map[string]any)
		msg, _ := c0["message"].(map[string]any)
		calls, _ := msg["tool_calls"].([]any)
		tc, _ := calls[0].(map[string]any)
		fn, _ := tc["function"].(map[string]any)
		id, _ := tc["id"].(string)
		name, _ := fn["name"].(string)
		return id, name
	case provider.SurfaceAnthropic:
		content, _ := m["content"].([]any)
		c0, _ := content[0].(map[string]any)
		id, _ := c0["id"].(string)
		name, _ := c0["name"].(string)
		return id, name
	default:
		out, _ := m["output"].([]any)
		c0, _ := out[0].(map[string]any)
		id, _ := c0["call_id"].(string)
		name, _ := c0["name"].(string)
		return id, name
	}
}

// upstreamToolCallOf asserts the upstream request carries the prior call and
// its result in the upstream wire's own shape.
func upstreamToolCallOf(t *testing.T, upstream provider.Surface, body []byte) (callName string, result string) {
	t.Helper()
	m := jsonMap(t, body)
	items := func(key string) []any {
		v, _ := m[key].([]any)
		return v
	}
	switch upstream {
	case provider.SurfaceOpenAI:
		msgs := items("messages")
		for _, it := range msgs {
			c0, _ := it.(map[string]any)
			if calls, ok := c0["tool_calls"].([]any); ok && len(calls) > 0 {
				tc, _ := calls[0].(map[string]any)
				fn, _ := tc["function"].(map[string]any)
				callName, _ = fn["name"].(string)
			}
			if c0["role"] == "tool" {
				result, _ = c0["content"].(string)
			}
		}
	case provider.SurfaceAnthropic:
		msgs := items("messages")
		for _, it := range msgs {
			c0, _ := it.(map[string]any)
			if blocks, ok := c0["content"].([]any); ok {
				for _, b := range blocks {
					bm, _ := b.(map[string]any)
					if bm["type"] == "tool_use" {
						n, _ := bm["name"].(string)
						callName = n
					}
					if bm["type"] == "tool_result" {
						result, _ = bm["content"].(string)
					}
				}
			}
		}
	default:
		in := items("input")
		for _, it := range in {
			c0, _ := it.(map[string]any)
			if c0["type"] == "function_call" {
				n, _ := c0["name"].(string)
				callName = n
			}
			if c0["type"] == "function_call_output" {
				result, _ = c0["output"].(string)
			}
		}
	}
	if callName == "" || result == "" {
		t.Fatalf("upstream body lost the tool loop (%s):\n%s", upstream, body)
	}
	return callName, result
}

func TestWireMatrixToolLoopCrossCells(t *testing.T) {
	for _, cell := range crossCells() {
		client, up := cell[0], cell[1]
		t.Run(string(client)+"-"+string(up), func(t *testing.T) {
			rec := &upstreamRecorder{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rec.calls++
				rec.path = r.URL.Path
				rec.body, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(toolCallResponse(up)))
			}))
			defer srv.Close()

			body := toolHistory(client)
			d := newDispatcher()
			d.SetRetries(0)
			req := httptest.NewRequest(http.MethodPost, "/v1/x", strings.NewReader(body))
			recout := httptest.NewRecorder()
			if _, res, err := d.ForwardCandidates(context.Background(), recout, req,
				[]Route{wireRoute(srv.URL, client, up)}, []byte(body)); err != nil || res.Status != 200 {
				t.Fatalf("forward: err=%v status=%d body=%s", err, res.Status, recout.Body.String())
			}

			// The replay reached the upstream in ITS shape: the prior call's
			// name and the result's content, both preserved.
			name, result := upstreamToolCallOf(t, up, rec.body)
			if name != "ls" || result != "a.txt" {
				t.Errorf("upstream tool loop = (%q,%q), want (ls,a.txt)", name, result)
			}
			// The new call came back with its PUBLIC identity on the client's
			// wire — never the upstream's internal item id.
			id, name2 := clientToolCallOf(t, client, recout.Body.Bytes())
			if id != "call_2" || name2 != "ls" {
				t.Errorf("client tool call = (%q,%q), want (call_2,ls)", id, name2)
			}
		})
	}
}

func TestWireMatrix429MixedWireFallback(t *testing.T) {
	// Hop 1: a chat-wire hop that answers 429. Hop 2: a Messages-wire hop
	// that succeeds. The client must receive the messages-derived response.
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited","type":"rate_limit_error"}}`))
			return
		}
		_, _ = w.Write([]byte(msgUpstreamResp))
	}))
	defer srv.Close()

	clientBody := `{"model":"daily","messages":[{"role":"user","content":"hi"}],"max_tokens":64}`
	d := newDispatcher()
	d.SetRetries(0)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(clientBody))
	recout := httptest.NewRecorder()
	hop1 := wireRoute(srv.URL, provider.SurfaceOpenAI, provider.SurfaceOpenAI)
	hop2 := wireRoute(srv.URL, provider.SurfaceOpenAI, provider.SurfaceAnthropic)
	// A combo's second candidate is a DIFFERENT hop: same account+model would
	// be parked by the 429 cooldown instead of being tried on the other wire.
	hop2.Account.ID, hop2.Account.Name, hop2.Account.Namespace = 2, "second", "second"
	hop2.Model = "second-model"
	served, res, err := d.ForwardCandidates(context.Background(), recout, req,
		[]Route{hop1, hop2}, []byte(clientBody))
	if err != nil || res.Status != 200 {
		t.Fatalf("err=%v status=%d body=%s", err, res.Status, recout.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Errorf("upstream calls = %d, want 2 (429 must hop)", calls)
	}
	if served.EffectiveWire() != provider.SurfaceAnthropic {
		t.Errorf("served wire = %s, want messages", served.EffectiveWire())
	}
	if got := responseTextOf(t, provider.SurfaceOpenAI, recout.Body.Bytes()); got != "hello" {
		t.Errorf("client text = %q, want hello", got)
	}
	if len(res.Attempts) != 1 || !strings.Contains(res.Attempts[0].Note, "429") {
		t.Errorf("attempt trail = %+v, want the 429 noted", res.Attempts)
	}
}

func TestWireMatrix400NoBlindFailover(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad request body","type":"invalid_request_error"}}`))
	}))
	defer srv.Close()

	clientBody := `{"model":"daily","messages":[{"role":"user","content":"hi"}],"max_tokens":64}`
	d := newDispatcher()
	d.SetRetries(0)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(clientBody))
	recout := httptest.NewRecorder()
	hop1 := wireRoute(srv.URL, provider.SurfaceOpenAI, provider.SurfaceAnthropic)
	hop2 := wireRoute(srv.URL, provider.SurfaceOpenAI, provider.SurfaceAnthropic)
	if _, res, err := d.ForwardCandidates(context.Background(), recout, req,
		[]Route{hop1, hop2}, []byte(clientBody)); err != nil || res.Status != 400 {
		t.Fatalf("err=%v status=%d body=%s", err, res.Status, recout.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("upstream calls = %d, want 1 (a 400 is the request's fault, not the hop's)", calls)
	}
	if !strings.Contains(recout.Body.String(), "bad request body") {
		t.Errorf("client lost the upstream's own words: %s", recout.Body.String())
	}
}

func TestWireMatrixLateStreamError(t *testing.T) {
	// A transcript that streams a delta and THEN fails. No retry may follow
	// (bytes already left), and the outcome must say failed — both for the
	// strict cell that surfaces the error frame and for the legacy cell whose
	// clients still see the historic synthesized close.
	for _, tc := range []struct {
		client, upstream provider.Surface
		wantClientText   string // a fragment the client must have received
		wantOutcome      string
	}{
		{provider.SurfaceOpenAI, provider.SurfaceAnthropic, "partial", StreamOutcomeFailed},
		{provider.SurfaceAnthropic, provider.SurfaceOpenAI, "partial", StreamOutcomeTruncated},
	} {
		t.Run(string(tc.client)+"-"+string(tc.upstream), func(t *testing.T) {
			var mu sync.Mutex
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				calls++
				mu.Unlock()
				w.Header().Set("Content-Type", "text/event-stream")
				if tc.upstream == provider.SurfaceOpenAI {
					// Legacy chat cut: bytes, then EOF without a terminal.
					_, _ = w.Write([]byte(`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}` + "\n\n"))
					return
				}
				// Messages source that fails late: an error frame mid-stream.
				_, _ = w.Write([]byte(`event: message_start` + "\n" +
					`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"upstream-model","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":4,"output_tokens":1}}}` + "\n\n" +
					`event: content_block_start` + "\n" +
					`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
					`event: content_block_delta` + "\n" +
					`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}` + "\n\n" +
					`event: error` + "\n" +
					`data: {"type":"error","error":{"type":"api_error","message":"upstream died late"}}` + "\n\n"))
			}))
			defer srv.Close()

			raw := clientBody(tc.client, false)
			body := raw[:len(raw)-1] + `,"stream":true}`
			d := newDispatcher()
			d.SetRetries(0)
			req := httptest.NewRequest(http.MethodPost, "/v1/x", strings.NewReader(body))
			recout := httptest.NewRecorder()
			_, res, ferr := d.ForwardCandidates(context.Background(), recout, req,
				[]Route{wireRoute(srv.URL, tc.client, tc.upstream)}, []byte(body))
			if ferr != nil {
				t.Fatalf("forward: %v", ferr)
			}
			// The status was already committed with the stream: it must stay
			// the upstream's own 200, and the outcome carries the truth.
			if res.Status != 200 {
				t.Fatalf("status = %d, want the committed 200", res.Status)
			}
			mu.Lock()
			defer mu.Unlock()
			if calls != 1 {
				t.Errorf("upstream calls = %d, want 1 (bytes already left; never retry)", calls)
			}
			if res.StreamOutcome != tc.wantOutcome {
				t.Errorf("stream outcome = %q, want %q", res.StreamOutcome, tc.wantOutcome)
			}
			if res.Err == "" {
				t.Errorf("the ledger err column must record the outcome")
			}
			if tc.wantOutcome == StreamOutcomeFailed && !strings.Contains(recout.Body.String(), "upstream died late") {
				t.Errorf("strict client must see the late error frame: %s", recout.Body.String())
			}
			if tc.wantOutcome == StreamOutcomeTruncated && !strings.Contains(recout.Body.String(), "partial") {
				t.Errorf("legacy client must still see the partial bytes: %s", recout.Body.String())
			}
		})
	}
}

func TestWireMatrixPreparationRefusalZeroSpend(t *testing.T) {
	// A chat request without max_tokens cannot become a Messages request
	// (that wire requires an explicit output limit): the refusal is typed,
	// names the field, and no upstream call is made.
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		_, _ = w.Write([]byte(`{"id":"c1","choices":[{"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	clientBody := `{"model":"daily","messages":[{"role":"user","content":"hi"}]}`
	d := newDispatcher()
	d.SetRetries(0)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(clientBody))
	recout := httptest.NewRecorder()
	_, _, err := d.ForwardCandidates(context.Background(), recout, req,
		[]Route{wireRoute(srv.URL, provider.SurfaceOpenAI, provider.SurfaceAnthropic)}, []byte(clientBody))
	var ufe *translate.UnsupportedFieldError
	if err == nil {
		t.Fatal("a messages-bound request without max_tokens must be refused")
	}
	if !errors.As(err, &ufe) {
		t.Fatalf("error = %T (%v), want *translate.UnsupportedFieldError", err, err)
	}
	if strings.Contains(recout.Body.String(), "hello") {
		t.Errorf("a refused request must not leak a successful body: %s", recout.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Errorf("upstream calls = %d, want 0 (preparation refusal budget zero)", calls)
	}
}

// The usage-convention guard is enforced in the codec path too, not only in
// the router: a hand-built route onto a Messages wire with an unstated
// convention cannot stream, even though the router would have refused it.
func TestUniversalCodecOptionsRequiresConvention(t *testing.T) {
	rt := testRoute("http://unused.invalid", provider.SurfaceOpenAI)
	rt.WireSurface = provider.SurfaceAnthropic
	rt.Account.UsageConvention = provider.UsageConventionAuto
	if _, err := universalCodecOptions(rt); err == nil {
		t.Fatal("an unstated convention must be refused for a Messages upstream")
	}
	rt.Account.UsageConvention = provider.UsageConventionAnthropic
	if _, err := universalCodecOptions(rt); err != nil {
		t.Fatalf("explicit convention refused: %v", err)
	}
}
