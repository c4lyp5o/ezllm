package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/store"
)

func testRoute(base string, surface provider.Surface) Route {
	return Route{
		Account: provider.Account{
			ID: 1, Name: "test", Namespace: "test", Kind: provider.KindOpenAICompatible,
			BaseURL: base, Enabled: true, RequiresSessionHeader: false,
		},
		KeyID: 1, KeyPlain: "sk-test-key", KeyHint: "sk…test",
		Model: "resolved-model", Alias: "test/resolved-model", Surface: surface,
	}
}

func newDispatcher() *Dispatcher {
	return NewDispatcher(&http.Client{Timeout: 30 * time.Second}, provider.NewRegistry(&http.Client{Timeout: 30 * time.Second}))
}

// ── SwapModel: the "preserve unknown fields" invariant ───────────────────────

func TestSwapModelPreservesUnknownFields(t *testing.T) {
	// Every field here is one we observed in live provider payloads; losing any
	// of them would silently change behaviour upstream.
	body := `{
	  "model": "old-model",
	  "messages": [{"role":"user","content":"hi"}],
	  "max_tokens": 100,
	  "stream": true,
	  "extra_body": {"enable_thinking": true, "nested": {"deep": [1,2,3]}},
	  "thinking": {"type":"enabled","budget_tokens":1024},
	  "enable_thinking": false,
	  "tools": [{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],
	  "tool_choice": {"type":"auto"},
	  "reasoning": {"effort":"high"},
	  "previous_response_id": "resp_abc123",
	  "prompt_cache_retention": "1h",
	  "max_tool_calls": 5,
	  "temperature": 0.7,
	  "top_p": 0.98,
	  "response_format": {"type":"json_object"},
	  "metadata": {"user_id":"u1"}
	}`
	out, old, err := SwapModel([]byte(body), "new-model")
	if err != nil {
		t.Fatal(err)
	}
	if old != "old-model" {
		t.Errorf("old model = %q", old)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	if m["model"] != "new-model" {
		t.Errorf("model not swapped: %v", m["model"])
	}
	for _, k := range []string{
		"messages", "max_tokens", "stream", "extra_body", "thinking", "enable_thinking",
		"tools", "tool_choice", "reasoning", "previous_response_id", "prompt_cache_retention",
		"max_tool_calls", "temperature", "top_p", "response_format", "metadata",
	} {
		if _, ok := m[k]; !ok {
			t.Errorf("field %q was LOST during model swap", k)
		}
	}
	// nested content survives intact
	eb, _ := m["extra_body"].(map[string]any)
	if eb["enable_thinking"] != true {
		t.Errorf("extra_body.enable_thinking mangled: %v", eb["enable_thinking"])
	}
	if m["previous_response_id"] != "resp_abc123" {
		t.Errorf("previous_response_id mangled: %v", m["previous_response_id"])
	}
}

// Large integers must not be mangled into floats (UseNumber guards this).
func TestSwapModelPreservesBigInts(t *testing.T) {
	body := `{"model":"m","max_tokens":9007199254740993,"user_id":12345678901234567890}`
	out, _, err := SwapModel([]byte(body), "m2")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "9007199254740993") {
		t.Errorf("big int mangled to float: %s", out)
	}
}

func TestSwapModelRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "not json", "[]", `"string"`, "null"} {
		if _, _, err := SwapModel([]byte(bad), "m"); err == nil {
			t.Errorf("SwapModel(%q) should fail", bad)
		}
	}
}

func TestIsStream(t *testing.T) {
	cases := map[string]bool{
		`{"stream":true}`:             true,
		`{"stream":false}`:            false,
		`{"model":"m"}`:               false,
		`{"stream":true,"model":"m"}`: true,
		`not json`:                    false,
	}
	for body, want := range cases {
		if got := IsStream([]byte(body)); got != want {
			t.Errorf("IsStream(%q) = %v, want %v", body, got, want)
		}
	}
}

// ── usage taps, against VERBATIM live payloads ──────────────────────────────

func TestTapSSELineOpenAIFinalChunk(t *testing.T) {
	// Verbatim shape captured live 2026-10-05 (P4, cached repeat).
	line := `data: {"id":"chatcmpl-x","object":"chat.completion.chunk","choices":[{"delta":{"content":"5"},"index":0}],"usage":{"prompt_tokens":2044,"completion_tokens":72,"total_tokens":2116,"prompt_tokens_details":{"cached_tokens":1792},"completion_tokens_details":{"reasoning_tokens":28}}}`
	u := tapSSELine([]byte(line), provider.SurfaceOpenAI)
	if u == nil {
		t.Fatal("usage not tapped")
	}
	// cached is INCLUDED in prompt_tokens -> cold in = 2044-1792 = 252
	if u.In != 252 || u.Out != 72 || u.CachedRead != 1792 || u.Reasoning != 28 {
		t.Errorf("normalized wrong: in=%d out=%d cr=%d rt=%d", u.In, u.Out, u.CachedRead, u.Reasoning)
	}
}

func TestTapSSELineOpenAIIntermediateChunksHaveNoUsage(t *testing.T) {
	for _, line := range []string{
		`data: {"choices":[{"delta":{"role":"assistant"},"index":0}]}`,
		`data: {"choices":[{"delta":{"content":"Hi"},"index":0}]}`,
		`data: [DONE]`,
		`data:`,
		`event: ping`,
		`: comment line`,
		``,
	} {
		if u := tapSSELine([]byte(line), provider.SurfaceOpenAI); u != nil {
			t.Errorf("line %q should not yield usage, got %+v", line, u)
		}
	}
}

func TestTapSSELineAnthropicMessageDelta(t *testing.T) {
	// Verbatim from live probe P2/P5.
	start := `data: {"type":"message_start","message":{"id":"msg_x","role":"assistant","model":"qwen3.8-flash","usage":{"input_tokens":252,"output_tokens":1,"cache_read_input_tokens":1792,"cache_creation_input_tokens":0}}}`
	delta := `data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"input_tokens":67,"output_tokens":32,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`

	us := tapSSELine([]byte(start), provider.SurfaceAnthropic)
	ud := tapSSELine([]byte(delta), provider.SurfaceAnthropic)
	if us == nil || ud == nil {
		t.Fatal("anthropic usage not tapped")
	}
	if us.CachedRead != 1792 || us.In != 252 {
		t.Errorf("message_start wrong: in=%d cr=%d", us.In, us.CachedRead)
	}
	merged := MergeUsage(us, ud)
	if merged.Out != 32 {
		t.Errorf("merged output = %d, want 32 (delta is authoritative)", merged.Out)
	}
	if merged.CachedRead != 1792 {
		t.Errorf("merged cached_read = %d, want 1792 (from message_start)", merged.CachedRead)
	}
	if merged.In != 67 {
		t.Errorf("merged in = %d, want 67", merged.In)
	}
}

func TestTapSSELineAnthropicIgnoresOtherEvents(t *testing.T) {
	for _, line := range []string{
		`event: ping`,
		`data: {"type":"ping"}`,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`,
		`data: {"type":"content_block_stop"}`,
		`data: {"type":"message_stop"}`,
	} {
		if u := tapSSELine([]byte(line), provider.SurfaceAnthropic); u != nil {
			t.Errorf("line %q should not yield usage: %+v", line, u)
		}
	}
}

func TestTapSSELineResponsesCompleted(t *testing.T) {
	// Verbatim from live probe P8 (streaming gpt-6-luna).
	line := `data: {"type":"response.completed","response":{"id":"resp_x","status":"completed","model":"gpt-6-luna","usage":{"input_tokens":12,"output_tokens":27,"total_tokens":39,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0},"output_tokens_details":{"reasoning_tokens":13}}}}`
	u := tapSSELine([]byte(line), provider.SurfaceResponses)
	if u == nil {
		t.Fatal("responses usage not tapped")
	}
	if u.In != 12 || u.Out != 27 || u.Reasoning != 13 {
		t.Errorf("wrong: in=%d out=%d rt=%d", u.In, u.Out, u.Reasoning)
	}
}

func TestTapSSELineResponsesCacheWrite(t *testing.T) {
	// The Responses surface is the ONLY one reporting cache_write (P6) — the
	// dashboard's "cached write" column depends on this tap.
	line := `data: {"type":"response.completed","response":{"usage":{"input_tokens":3000,"output_tokens":40,"input_tokens_details":{"cached_tokens":2048,"cache_write_tokens":512}}}}`
	u := tapSSELine([]byte(line), provider.SurfaceResponses)
	if u == nil {
		t.Fatal("no usage")
	}
	if u.CachedWrite != 512 {
		t.Errorf("cached_write = %d, want 512", u.CachedWrite)
	}
	if u.CachedRead != 2048 || u.In != 952 {
		t.Errorf("cr=%d in=%d, want 2048/952", u.CachedRead, u.In)
	}
}

func TestTapJSONUsageNonStreamed(t *testing.T) {
	// Verbatim from live probe P6 (non-streamed gpt-6-luna on /v1/responses).
	body := `{"id":"resp_029eb759","object":"response","status":"completed","model":"gpt-6-luna","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}`
	u := tapJSONUsage([]byte(body), provider.SurfaceResponses)
	if u == nil || u.In != 10 || u.Out != 5 {
		t.Fatalf("responses non-stream tap wrong: %+v", u)
	}
	// Anthropic non-streamed (live probe P9, tool use).
	anth := `{"type":"message","id":"msg_x","role":"assistant","stop_reason":"tool_use","content":[{"type":"thinking"},{"type":"tool_use","id":"toolu_dd55","name":"get_weather","input":{"city":"Tokyo"}}],"usage":{"input_tokens":315,"output_tokens":73,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`
	ua := tapJSONUsage([]byte(anth), provider.SurfaceAnthropic)
	if ua == nil || ua.In != 315 || ua.Out != 73 {
		t.Fatalf("anthropic non-stream tap wrong: %+v", ua)
	}
	if tapJSONUsage([]byte(`{"error":"x"}`), provider.SurfaceOpenAI) != nil {
		t.Error("error body must not yield usage")
	}
}

// ── end-to-end against a fake upstream ──────────────────────────────────────

func TestForwardNonStreamedWritesBodyAndTapsUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("upstream path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test-key" {
			t.Errorf("upstream Authorization = %q", got)
		}
		// the model must have been swapped before it reached us
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		json.Unmarshal(body, &m)
		if m["model"] != "resolved-model" {
			t.Errorf("upstream saw model %v", m["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","model":"resolved-model","choices":[{"message":{"content":"OK"}}],"usage":{"prompt_tokens":2044,"completion_tokens":72,"prompt_tokens_details":{"cached_tokens":1792}}}`))
	}))
	defer upstream.Close()

	d := newDispatcher()
	reqBody, _, _ := SwapModel([]byte(`{"model":"whatever","messages":[{"role":"user","content":"hi"}]}`), "resolved-model")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(reqBody)))
	res, err := d.Forward(context.Background(), rec, req, testRoute(upstream.URL, provider.SurfaceOpenAI), reqBody)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != 200 {
		t.Errorf("status = %d", res.Status)
	}
	if res.Stream {
		t.Error("should not be marked as stream")
	}
	if res.Usage == nil || res.Usage.In != 252 || res.Usage.CachedRead != 1792 {
		t.Errorf("usage tap wrong: %+v", res.Usage)
	}
	if !strings.Contains(rec.Body.String(), `"content":"OK"`) {
		t.Errorf("client did not receive upstream body: %s", rec.Body.String())
	}
	if res.Totalms == nil || *res.Totalms < 0 {
		t.Error("total_ms not recorded")
	}
}

// THE streaming invariant: bytes arrive incrementally, unbuffered, and the
// client sees exactly what upstream sent.
func TestForwardStreamedIsIncrementalAndLossless(t *testing.T) {
	chunks := []string{
		`data: {"choices":[{"delta":{"role":"assistant"}}]}`,
		`data: {"choices":[{"delta":{"content":"1"}}]}`,
		`data: {"choices":[{"delta":{"content":"2"}}]}`,
		`data: {"choices":[{"delta":{"content":"3"}}],"usage":{"prompt_tokens":100,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":0}}}`,
		`data: [DONE]`,
	}
	var flushCount int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(200)
		f, _ := w.(http.Flusher)
		for _, c := range chunks {
			fmt.Fprintf(w, "%s\n\n", c)
			if f != nil {
				f.Flush()
			}
			time.Sleep(25 * time.Millisecond) // stagger so buffering would be detectable
		}
	}))
	defer upstream.Close()

	d := newDispatcher()
	reqBody := []byte(`{"model":"resolved-model","stream":true,"messages":[{"role":"user","content":"count"}]}`)

	// A pipe-backed ResponseWriter lets us observe arrival times.
	pr, pw := io.Pipe()
	w := &pipeWriter{header: http.Header{}, body: pw, t: t}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(reqBody)))

	type arrival struct {
		n  int
		at time.Duration
	}
	arrivals := make(chan arrival, 64)
	start := time.Now()
	go func() {
		sc := bufio.NewScanner(pr)
		n := 0
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "data:") {
				n++
				arrivals <- arrival{n, time.Since(start)}
			}
		}
		close(arrivals)
	}()

	res, err := d.Forward(context.Background(), w, req, testRoute(upstream.URL, provider.SurfaceOpenAI), reqBody)
	if err != nil {
		t.Fatal(err)
	}
	pw.Close()

	var got []arrival
	for a := range arrivals {
		got = append(got, a)
	}
	if len(got) != len(chunks) {
		t.Fatalf("client received %d data lines, upstream sent %d", len(got), len(chunks))
	}
	// Incremental, not buffered: the first chunk must arrive well before the last.
	if got[0].at >= got[len(got)-1].at {
		t.Errorf("stream appears buffered: first=%v last=%v", got[0].at, got[len(got)-1].at)
	}
	if got[len(got)-1].at < 80*time.Millisecond {
		t.Errorf("all chunks arrived in %v — upstream staggered them by 25ms each, so this indicates buffering", got[len(got)-1].at)
	}
	if flushCount > 0 {
		t.Logf("flushes: %d", flushCount)
	}
	if w.flushes < len(chunks) {
		t.Errorf("only %d flushes for %d chunks — response was buffered", w.flushes, len(chunks))
	}
	if res.Usage == nil || res.Usage.In != 100 || res.Usage.Out != 3 {
		t.Errorf("streamed usage tap wrong: %+v", res.Usage)
	}
	if res.TTFTms == nil {
		t.Error("ttft not recorded")
	} else if *res.TTFTms < 1 {
		// Floor of 1: a truncated 0 is indistinguishable from "unmeasured" in
		// the dashboard, so sub-millisecond TTFT must clamp up, not down.
		t.Errorf("ttft = %d, want >= 1 (sub-ms must clamp, not truncate to 0)", *res.TTFTms)
	}
	if res.Totalms != nil && *res.Totalms > 1 && *res.TTFTms > *res.Totalms {
		t.Errorf("ttft %d > total %d — impossible", *res.TTFTms, *res.Totalms)
	}
	if !res.Stream {
		t.Error("Stream flag not set")
	}
}

// pipeWriter is a ResponseWriter that counts Flush calls and writes to a pipe.
type pipeWriter struct {
	header  http.Header
	body    io.Writer
	status  int
	flushes int
	t       *testing.T
}

func (w *pipeWriter) Header() http.Header { return w.header }
func (w *pipeWriter) WriteHeader(code int) {
	w.status = code
}
func (w *pipeWriter) Write(b []byte) (int, error) { return w.body.Write(b) }
func (w *pipeWriter) Flush() {
	w.flushes++
	if f, ok := w.body.(interface{ Flush() error }); ok {
		_ = f.Flush()
	}
}

// Upstream errors must be relayed verbatim with their status, not swallowed.
func TestForwardRelaysUpstreamError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		w.Write([]byte(`{"error":{"type":"rate_limit","message":"slow down"}}`))
	}))
	defer upstream.Close()

	d := newDispatcher()
	reqBody := []byte(`{"model":"resolved-model","messages":[]}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(reqBody)))
	res, _ := d.Forward(context.Background(), rec, req, testRoute(upstream.URL, provider.SurfaceOpenAI), reqBody)
	if res.Status != 429 {
		t.Errorf("status = %d, want 429", res.Status)
	}
	if rec.Code != 429 {
		t.Errorf("client got %d, want 429", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "slow down") {
		t.Errorf("upstream error body not relayed: %s", rec.Body.String())
	}
}

// Anthropic surface must hit /messages with x-api-key, not /chat/completions.
func TestForwardAnthropicSurfaceRouting(t *testing.T) {
	var gotPath, gotAPIKey, gotVersion, gotBearer string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		gotBearer = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"type":"message","usage":{"input_tokens":315,"output_tokens":73,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`))
	}))
	defer upstream.Close()

	d := newDispatcher()
	reqBody := []byte(`{"model":"resolved-model","messages":[{"role":"user","content":"hi"}]}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(reqBody)))
	res, err := d.Forward(context.Background(), rec, req, testRoute(upstream.URL, provider.SurfaceAnthropic), reqBody)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/messages" {
		t.Errorf("upstream path = %s, want /messages", gotPath)
	}
	if gotAPIKey != "sk-test-key" {
		t.Errorf("x-api-key = %q", gotAPIKey)
	}
	if gotVersion == "" {
		t.Error("anthropic-version missing")
	}
	_ = gotBearer
	if res.Usage == nil || res.Usage.In != 315 || res.Usage.Out != 73 {
		t.Errorf("anthropic usage wrong: %+v", res.Usage)
	}
}

// opencode-go must get a generated session header, and a client-supplied one
// must be stripped rather than forwarded.
func TestForwardStripsClientSessionHeader(t *testing.T) {
	var gotSession, gotRequest string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSession = r.Header.Get("x-opencode-session")
		gotRequest = r.Header.Get("x-opencode-request")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	rt := testRoute(upstream.URL, provider.SurfaceOpenAI)
	rt.Account.Kind = provider.KindOpenCodeGo
	rt.Account.RequiresSessionHeader = true

	d := newDispatcher()
	reqBody := []byte(`{"model":"resolved-model","messages":[]}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(reqBody)))
	req.Header.Set("x-opencode-session", "client-evil")
	req.Header.Set("x-opencode-request", "client-evil-req")
	req.Header.Set("Authorization", "Bearer ***")
	req.Header.Set("Cookie", "session=steal-me")

	if _, err := d.Forward(context.Background(), rec, req, rt, reqBody); err != nil {
		t.Fatal(err)
	}
	if gotSession == "" {
		t.Error("no session header generated — upstream would 400 MissingSessionID")
	}
	if gotSession == "client-evil" {
		t.Error("client-supplied session header was forwarded")
	}
	if gotRequest == "client-evil-req" {
		t.Error("client-supplied request id was forwarded")
	}
}

// The client's own Authorization/Cookie must never reach upstream (the adapter
// applies the provider credential instead).
func TestForwardStripsClientCredentials(t *testing.T) {
	var gotAuth, gotCookie, gotAPIKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCookie = r.Header.Get("Cookie")
		gotAPIKey = r.Header.Get("x-api-key")
		w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	d := newDispatcher()
	reqBody := []byte(`{"model":"resolved-model","messages":[]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(reqBody)))
	req.Header.Set("Authorization", "Bearer ***")
	req.Header.Set("Cookie", "session=secret")
	req.Header.Set("x-api-key", "client-key")
	req.Header.Set("X-Custom-Ok", "keep-me")

	var gotCustom string
	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCookie = r.Header.Get("Cookie")
		gotAPIKey = r.Header.Get("x-api-key")
		gotCustom = r.Header.Get("X-Custom-Ok")
		w.Write([]byte(`{}`))
	})

	rec := httptest.NewRecorder()
	if _, err := d.Forward(context.Background(), rec, req, testRoute(upstream.URL, provider.SurfaceOpenAI), reqBody); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer sk-test-key" {
		t.Errorf("Authorization = %q, want the PROVIDER key", gotAuth)
	}
	if gotCookie != "" {
		t.Errorf("client Cookie forwarded upstream: %q", gotCookie)
	}
	if gotAPIKey != "" {
		t.Errorf("client x-api-key forwarded on an OpenAI surface: %q", gotAPIKey)
	}
	if gotCustom != "keep-me" {
		t.Errorf("benign custom header was stripped: %q", gotCustom)
	}
}

// Provenance headers (P12) must be captured for the ledger.
func TestForwardCapturesProvenanceHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-opencode-endpoint-id", "alibaba-us")
		w.Header().Set("x-opencode-upstream-model-id", "qwen3.8-flash")
		w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()
	d := newDispatcher()
	reqBody := []byte(`{"model":"resolved-model","messages":[]}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(reqBody)))
	res, err := d.Forward(context.Background(), rec, req, testRoute(upstream.URL, provider.SurfaceOpenAI), reqBody)
	if err != nil {
		t.Fatal(err)
	}
	if res.EndpointID != "alibaba-us" || res.UpstreamModel != "qwen3.8-flash" {
		t.Errorf("provenance not captured: %+v", res)
	}
}

// A malformed SSE line must not break the stream or panic.
func TestStreamCopySurvivesGarbageLines(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		f, _ := w.(http.Flusher)
		for _, l := range []string{
			"data: {not json at all",
			"data: ",
			": comment",
			"data: {\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2}}",
			"data: [DONE]",
		} {
			fmt.Fprintf(w, "%s\n\n", l)
			if f != nil {
				f.Flush()
			}
		}
	}))
	defer upstream.Close()

	d := newDispatcher()
	reqBody := []byte(`{"model":"m","stream":true,"messages":[]}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(reqBody)))
	res, err := d.Forward(context.Background(), rec, req, testRoute(upstream.URL, provider.SurfaceOpenAI), reqBody)
	if err != nil {
		t.Fatalf("garbage SSE must not error the request: %v", err)
	}
	if res.Usage == nil || res.Usage.In != 5 {
		t.Errorf("usage should still be tapped despite garbage lines: %+v", res.Usage)
	}
	if !strings.Contains(rec.Body.String(), "not json at all") {
		t.Error("garbage line was not forwarded byte-for-byte")
	}
}

func TestMergeUsageNilHandling(t *testing.T) {
	u := &store.Usage{In: 1, Out: 2}
	if MergeUsage(nil, nil) != nil {
		t.Error("nil+nil should be nil")
	}
	if got := MergeUsage(nil, u); got != u {
		t.Error("nil+u should be u")
	}
	if got := MergeUsage(u, nil); got != u {
		t.Error("u+nil should be u")
	}
}
