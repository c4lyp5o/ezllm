package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// An audio response over the cap is refused, not cut and passed on as broken
// JSON with a 200.
func TestGeminiAudioResponseRefusesOversize(t *testing.T) {
	big := bytes.Repeat([]byte("a"), maxBodyBytes+1)
	resp := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(big))}
	if err := geminiAudioResponse(resp, provider.SurfaceAudioTTS); err == nil {
		t.Fatal("an oversized audio response was accepted")
	}
}

// Client address and browser-page headers describe the hop to ezllm and
// never reach a provider.
func TestForwardingHeadersAreStripped(t *testing.T) {
	src, _ := http.NewRequest("POST", "http://x", nil)
	for _, h := range []string{"X-Forwarded-For", "X-Real-Ip", "Forwarded", "Cf-Connecting-Ip", "Referer", "Sec-Fetch-Site"} {
		src.Header.Set(h, "v")
	}
	src.Header.Set("X-Custom", "keep")
	dst, _ := http.NewRequest("POST", "http://y", nil)
	copyClientHeaders(dst, src, nil)
	for h := range src.Header {
		if h != "X-Custom" && dst.Header.Get(h) != "" {
			t.Errorf("%s reached the upstream", h)
		}
	}
	if dst.Header.Get("X-Custom") != "keep" {
		t.Error("an ordinary header was dropped")
	}
}

func TestIsCloudflareChecksTheHost(t *testing.T) {
	yes := Route{Account: provider.Account{BaseURL: "https://api.cloudflare.com/client/v4/accounts/x/ai/v1"}}
	no := Route{Account: provider.Account{BaseURL: "https://proxy.example.com/api.cloudflare.com/v1"}}
	if !isCloudflare(yes) || isCloudflare(no) {
		t.Fatal("host check wrong")
	}
}

// A later zero output count must not wipe a real count reported earlier; it
// still replaces message_start's placeholder.
func TestMergeUsageKeepsRealOutputCount(t *testing.T) {
	dst := map[string]any{"output_tokens": float64(250)}
	mergeUsageRaw(dst, map[string]any{"output_tokens": float64(0)})
	if dst["output_tokens"] != float64(250) {
		t.Fatalf("output_tokens = %v, want 250 kept", dst["output_tokens"])
	}
	ph := map[string]any{"output_tokens": float64(1)}
	mergeUsageRaw(ph, map[string]any{"output_tokens": float64(0)})
	if ph["output_tokens"] != float64(0) {
		t.Fatalf("placeholder not replaced: %v", ph["output_tokens"])
	}
}

// Some gateways answer an error to a streaming request as text/event-stream.
// On a translated hop that body still reaches the client as its own error
// envelope, not run through the stream translator.
func TestTranslatedStreamErrorIsReshaped(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"bad thing"}}`))
	}))
	defer upstream.Close()
	clientBody := `{"model":"daily","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	d := newDispatcher()
	d.SetRetries(0)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(clientBody))
	rec := httptest.NewRecorder()
	if _, _, err := d.ForwardCandidates(context.Background(), rec, req,
		[]Route{translatedRoute(upstream.URL, "step-5-preview-free")}, []byte(clientBody)); err != nil {
		t.Fatal(err)
	}
	var env struct {
		Type  string `json:"type"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || env.Type != "error" || !strings.Contains(env.Error.Message, "bad thing") {
		t.Fatalf("client body = %q (err %v), want an anthropic error envelope", rec.Body.String(), err)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

// A blank line comes first, so a half-written event cannot swallow the error.
func TestStreamErrorFrameStartsOnItsOwn(t *testing.T) {
	var b bytes.Buffer
	b.WriteString(`data: {"partial`)
	writeStreamErrorFrame(&b, provider.SurfaceChat, "upstream connection lost")
	if !strings.Contains(b.String(), "\n\ndata: {\"error\"") {
		t.Fatalf("frame not separated: %q", b.String())
	}
}

// Model IDs go into Cloudflare and Gemini URL paths: real IDs keep their
// shape, and one that tries to climb the path or add a query cannot.
func TestModelPath(t *testing.T) {
	for in, want := range map[string]string{
		"@cf/meta/llama-3.1-8b-instruct": "@cf/meta/llama-3.1-8b-instruct",
		"models/gemini-2.5-flash":        "models/gemini-2.5-flash",
		"../../admin?x=1":                "%2E%2E/%2E%2E/admin%3Fx=1",
		"a#b":                            "a%23b",
	} {
		if got := modelPath(in); got != want {
			t.Errorf("modelPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGeminiAudioURLEscapesTheModel(t *testing.T) {
	rt := Route{Account: provider.Account{BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai"}, Model: "../x?y"}
	if got := geminiAudioURL(rt); strings.Contains(got, "/../") || strings.Contains(got, "?") {
		t.Fatalf("url = %q, the model escaped its path", got)
	}
}

// 408 and 425 share the operator's server-error cooldown, as the code comment
// always said; they used to get the fixed five minutes.
func TestTimeoutStatusesUseTheServerErrorCooldown(t *testing.T) {
	d := newDispatcher()
	d.ConfigureCooldown(0, 15*time.Second, 1)
	for _, st := range []int{http.StatusRequestTimeout, http.StatusTooEarly, http.StatusBadGateway} {
		if oc := d.classifyHopPolicy(context.Background(), nil, st, http.Header{}, true); oc.cooldown != 15*time.Second {
			t.Errorf("%d: cooldown %s, want 15s", st, oc.cooldown)
		}
	}
}

// The cooldown map is swept of finished parks once it is full, instead of
// growing with every model that never answers.
func TestCooldownMapIsBounded(t *testing.T) {
	d := newDispatcher()
	now := time.Now()
	d.cooldowns = map[string]cooldownState{}
	for i := 0; i < maxCooldownEntries; i++ {
		d.cooldowns[strconv.Itoa(i)] = cooldownState{until: now.Add(-time.Minute)}
	}
	rt := testRoute("http://x", provider.SurfaceChat)
	d.recordHop(rt, hopOutcome{failed: true, cooldown: time.Minute, reason: "502"}, now)
	if n := len(d.cooldowns); n > 10 {
		t.Fatalf("cooldown map holds %d entries after a sweep, want the expired ones gone", n)
	}
}

// Every hop skipped for context size says so, instead of "five-minute
// cooldown" when nothing is cooling down.
func TestAllHopsTooSmallSaysSo(t *testing.T) {
	d := newDispatcher()
	rt := testRoute("http://127.0.0.1:1", provider.SurfaceChat)
	rt.Account.MaxContextTokens = 1
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"a long enough prompt"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	_, _, err := d.ForwardCandidates(context.Background(), httptest.NewRecorder(), req, []Route{rt}, body)
	if err == nil || !strings.Contains(err.Error(), "context window") {
		t.Fatalf("err = %v, want the context window named", err)
	}
}
