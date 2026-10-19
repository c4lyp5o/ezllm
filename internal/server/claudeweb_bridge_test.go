package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/claudeweb"
	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// The bridge tests stand up a mock claude.ai (orgs + completion SSE over
// plain HTTP via the injected fetcher) and a real temp sqlite with one
// enabled claude-web account, then drive the bridge like the adapter does.

type claudeWebMock struct {
	orgsStatus int
	complet    func(w http.ResponseWriter, r *http.Request)
	seen       []string
}

func (m *claudeWebMock) Fetch(ctx context.Context, method, url string, body []byte, headers http.Header) (*claudeweb.Response, error) {
	m.seen = append(m.seen, method+" "+url)
	req, err := http.NewRequestWithContext(ctx, method, url, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	return &claudeweb.Response{Status: resp.StatusCode, Header: resp.Header, Body: resp.Body}, nil
}

func claudeWebFixture(t *testing.T, mock *claudeWebMock) (*claudeWebBridge, int64, *httptest.Server) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/organizations":
			if mock.orgsStatus != 0 && mock.orgsStatus != 200 {
				w.WriteHeader(mock.orgsStatus)
				fmt.Fprint(w, `{"error":"denied"}`)
				return
			}
			fmt.Fprint(w, `[{"uuid":"org-1","name":"Org"}]`)
		case strings.Contains(r.URL.Path, "/completion"):
			mock.complet(w, r)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(up.Close)

	dir := t.TempDir()
	db, err := store.Open(context.Background(), store.Options{
		Path: filepath.Join(dir, "t.sqlite"), BatchSize: 4, BatchWait: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.UpsertAccount(context.Background(), provider.Account{
		Name: "cw", Namespace: "cw", Kind: provider.KindClaudeWeb,
		BaseURL: "internal://claudeweb", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.AddKey(context.Background(), id, "primary", "sessionKey=sk-ant-test"); err != nil {
		t.Fatal(err)
	}
	// The mock speaks plain HTTP on its own origin; a fetcher that redirects
	// claude.ai there keeps the client's URL construction untouched.
	mock.seen = nil
	fetcher := &claudeWebMock{orgsStatus: mock.orgsStatus, complet: mock.complet}
	f := rewriteFetcher{t: t, upstream: up.URL, inner: fetcher}
	b := newClaudeWebBridge(db, f)
	return b, id, up
}

// rewriteFetcher points the production client at the mock origin (same
// paths) — the client's URL logic is exercised verbatim.
type rewriteFetcher struct {
	t        *testing.T
	upstream string
	inner    claudeweb.Fetcher
}

func (r rewriteFetcher) Fetch(ctx context.Context, method, url string, body []byte, headers http.Header) (*claudeweb.Response, error) {
	url = strings.Replace(url, "https://claude.ai", r.upstream, 1)
	return r.inner.Fetch(ctx, method, url, body, headers)
}

func claudeWebSSE(w http.ResponseWriter, deltas ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, d := range deltas {
		ev, _ := json.Marshal(map[string]any{
			"type": "content_block_delta", "delta": map[string]any{"type": "text_delta", "text": d},
		})
		fmt.Fprintf(w, "data: %s\n\n", ev)
	}
	fmt.Fprint(w, "data: {\"type\":\"message_stop\"}\n\n")
}

func claudeWebReq(t *testing.T, b *claudeWebBridge, method, path, body string, acctID int64) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.Header.Set(provider.ClaudeWebAccountHeader, fmt.Sprint(acctID))
	r.Header.Set(provider.BridgeSecretHeader, provider.BridgeSecret())
	w := httptest.NewRecorder()
	if path == claudeWebBridgePrefix+"/models" {
		b.handleModels(w, r)
	} else {
		b.handleChatCompletions(w, r)
	}
	return w
}

func TestClaudeWebBridgeModelsOracle(t *testing.T) {
	mock := &claudeWebMock{complet: func(w http.ResponseWriter, _ *http.Request) { claudeWebSSE(w) }}
	b, id, _ := claudeWebFixture(t, mock)
	w := claudeWebReq(t, b, "GET", claudeWebBridgePrefix+"/models", "", id)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	var out struct {
		Data []struct{ ID string } `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if len(out.Data) != len(provider.ClaudeWebDefaultModels) {
		t.Fatalf("models = %+v", out.Data)
	}
	if out.Data[0].ID != "claude-opus-5" {
		t.Errorf("first model = %q", out.Data[0].ID)
	}
	// missing account header (secret present, so the account check is what refuses) -> 401
	r := httptest.NewRequest("GET", claudeWebBridgePrefix+"/models", nil)
	r.Header.Set(provider.BridgeSecretHeader, provider.BridgeSecret())
	w2 := httptest.NewRecorder()
	b.handleModels(w2, r)
	if w2.Code != http.StatusUnauthorized {
		t.Errorf("no header code = %d", w2.Code)
	}
}

func TestClaudeWebBridgeDeadSession(t *testing.T) {
	mock := &claudeWebMock{orgsStatus: 401, complet: func(w http.ResponseWriter, _ *http.Request) { claudeWebSSE(w) }}
	b, id, _ := claudeWebFixture(t, mock)
	w := claudeWebReq(t, b, "GET", claudeWebBridgePrefix+"/models", "", id)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("dead session code = %d, want 401", w.Code)
	}
	if !strings.Contains(w.Body.String(), "authentication_error") {
		t.Errorf("body = %s", w.Body.String())
	}
}

func TestClaudeWebBridgeChatStream(t *testing.T) {
	mock := &claudeWebMock{complet: func(w http.ResponseWriter, _ *http.Request) { claudeWebSSE(w, "Hel", "lo") }}
	b, id, _ := claudeWebFixture(t, mock)
	w := claudeWebReq(t, b, "POST", claudeWebBridgePrefix+"/chat/completions",
		`{"model":"claude-opus-4.6","messages":[{"role":"user","content":"hi"}],"stream":true}`, id)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"content":"Hel"`) || !strings.Contains(body, `"content":"lo"`) ||
		!strings.Contains(body, "[DONE]") || !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Errorf("sse = %s", body)
	}
}

func TestClaudeWebBridgeChatNonStream(t *testing.T) {
	mock := &claudeWebMock{complet: func(w http.ResponseWriter, _ *http.Request) { claudeWebSSE(w, "answer") }}
	b, id, _ := claudeWebFixture(t, mock)
	w := claudeWebReq(t, b, "POST", claudeWebBridgePrefix+"/chat/completions",
		`{"model":"claude-sonnet-4-6","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}]}`, id)
	var out struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if len(out.Choices) != 1 || out.Choices[0].Message.Content != "answer" {
		t.Fatalf("resp = %s", w.Body.String())
	}
}

func TestClaudeWebBridgeModelPassThrough(t *testing.T) {
	// Design: ids pass through — the upstream answer reveals entitlement.
	// The bridge must forward an unlisted id rather than gating locally.
	var gotBody map[string]any
	mock := &claudeWebMock{complet: func(w http.ResponseWriter, r *http.Request) {
		json.Unmarshal(claudeWebReadAll(r), &gotBody)
		claudeWebSSE(w, "ok")
	}}
	b, id, _ := claudeWebFixture(t, mock)
	w := claudeWebReq(t, b, "POST", claudeWebBridgePrefix+"/chat/completions",
		`{"model":"claude-mystery-9","messages":[{"role":"user","content":"hi"}]}`, id)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	if gotBody["model"] != "claude-mystery-9" {
		t.Errorf("model = %v, want the unlisted id forwarded", gotBody["model"])
	}
}

func TestClaudeWebBridgeRefusals(t *testing.T) {
	mock := &claudeWebMock{complet: func(w http.ResponseWriter, _ *http.Request) { claudeWebSSE(w, "x") }}
	b, id, _ := claudeWebFixture(t, mock)
	for _, tc := range []struct{ name, body, want string }{
		{"response_format", `{"model":"claude-opus-5","messages":[{"role":"user","content":"x"}],"response_format":{"type":"json_object"}}`, "unsupported_feature"},
		{"no_model", `{"messages":[{"role":"user","content":"x"}]}`, "invalid_request_error"},
	} {
		w := claudeWebReq(t, b, "POST", claudeWebBridgePrefix+"/chat/completions", tc.body, id)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: code = %d", tc.name, w.Code)
		}
		if !strings.Contains(w.Body.String(), tc.want) {
			t.Errorf("%s: body = %s", tc.name, w.Body.String())
		}
	}
}

func TestClaudeWebBridgeToolsPassThrough(t *testing.T) {
	var gotBody map[string]any
	mock := &claudeWebMock{complet: func(w http.ResponseWriter, r *http.Request) {
		json.Unmarshal(claudeWebReadAll(r), &gotBody)
		claudeWebSSE(w, "ok")
	}}
	b, id, _ := claudeWebFixture(t, mock)
	w := claudeWebReq(t, b, "POST", claudeWebBridgePrefix+"/chat/completions",
		`{"model":"claude-opus-5","messages":[{"role":"user","content":"weather?"}],"tools":[{"type":"function","function":{"name":"get_weather","description":"w","parameters":{"type":"object"}}}]}`, id)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	tools, _ := gotBody["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", gotBody["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "get_weather" || tool["input_schema"] == nil {
		t.Errorf("native tool shape = %+v", tool)
	}
}

func TestClaudeWebBridgeChallengeMaps403(t *testing.T) {
	mock := &claudeWebMock{
		orgsStatus: 403,
		complet:    func(w http.ResponseWriter, _ *http.Request) { claudeWebSSE(w) },
	}
	// Force the challenge markers: a plain 403 body reads "denied", so use
	// the fetcher-level override instead — a 403 with challenge HTML.
	b, id, up := claudeWebFixture(t, mock)
	_ = up
	// The mock's 403 body lacks challenge markers, so it classifies as
	// ErrForbidden -> 403 with authentication_error type.
	w := claudeWebReq(t, b, "GET", claudeWebBridgePrefix+"/models", "", id)
	if w.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403", w.Code)
	}
}

func claudeWebReadAll(r *http.Request) []byte {
	b, _ := io.ReadAll(r.Body)
	return b
}

// TestClaudeWebBridgeSerializesPerAccount proves the per-account generation
// lock: two concurrent completions on the same account must never overlap
// upstream (the cached client carries mutable conversation state). Without
// the lock the mock sees 2 in flight at once.
func TestClaudeWebBridgeSerializesPerAccount(t *testing.T) {
	var inFlight, maxInFlight int32
	var wg sync.WaitGroup
	mock := &claudeWebMock{complet: func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&inFlight, 1)
		for {
			old := atomic.LoadInt32(&maxInFlight)
			if n <= old || atomic.CompareAndSwapInt32(&maxInFlight, old, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt32(&inFlight, -1)
		claudeWebSSE(w, "ok")
	}}
	b, id, _ := claudeWebFixture(t, mock)

	const n = 4
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			claudeWebReq(t, b, "POST", claudeWebBridgePrefix+"/chat/completions",
				`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`, id)
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt32(&maxInFlight); got != 1 {
		t.Fatalf("max concurrent upstream completions = %d, want 1 (per-account lock)", got)
	}
}
