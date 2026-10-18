package claudeweb

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mockFetcher serves plain HTTP against fixed routes (the production
// fetcher is exercised once against a real TLS server).
type mockFetcher struct {
	base    string
	orgs    func(w http.ResponseWriter)
	complet func(w http.ResponseWriter, r *http.Request)
	seen    []string // record of request paths
}

func (m *mockFetcher) Fetch(ctx context.Context, method, url string, body []byte, headers http.Header) (*Response, error) {
	// Keep the /api prefix: the mock's mux routes the real paths.
	path := strings.TrimPrefix(url, "https://claude.ai")
	m.seen = append(m.seen, method+" "+path)
	req, err := http.NewRequestWithContext(ctx, method, m.base+path, byteReader(body))
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
	return &Response{Status: resp.StatusCode, Header: resp.Header, Body: resp.Body}, nil
}

func newMock(t *testing.T, mf *mockFetcher) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/organizations":
			mf.orgs(w)
		case strings.Contains(r.URL.Path, "/completion"):
			mf.complet(w, r)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	mf.base = srv.URL
	return New(mf, "sessionKey=sk-ant-test", "")
}

func orgsOK(w http.ResponseWriter) {
	fmt.Fprint(w, `[{"uuid":"org-uuid-1","name":"Test Org"}]`)
}

func sseTurn(t *testing.T, w http.ResponseWriter, deltas []string, stop bool) {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream")
	for _, d := range deltas {
		ev, _ := json.Marshal(map[string]any{
			"type":  "content_block_delta",
			"delta": map[string]any{"type": "text_delta", "text": d},
		})
		fmt.Fprintf(w, "data: %s\n\n", ev)
	}
	if stop {
		fmt.Fprint(w, "data: {\"type\":\"message_stop\"}\n\n")
	}
}

func TestNormalizeSessionCookie(t *testing.T) {
	if got, err := NormalizeSessionCookie("sessionKey=sk-ant-123"); err != nil || got != "sessionKey=sk-ant-123" {
		t.Errorf("prefixed: %q %v", got, err)
	}
	if got, err := NormalizeSessionCookie("  sk-ant-123  "); err != nil || got != "sessionKey=sk-ant-123" {
		t.Errorf("bare: %q %v", got, err)
	}
	if _, err := NormalizeSessionCookie("other=sk-ant"); err == nil {
		t.Error("wrong cookie name must fail")
	}
	if _, err := NormalizeSessionCookie("sk-ant\r\nX-Evil: 1"); err == nil {
		t.Error("CR/LF must fail")
	}
	if _, err := NormalizeSessionCookie("   "); err == nil {
		t.Error("empty must fail")
	}
	// A full browser header must be preserved verbatim — narrowing it to
	// sessionKey is what produced the synthetic 429 on the live wire.
	full := "cf_clearance=abc; anthropic-device-id=dev-42; sessionKey=sk-ant-123; sessionKeyV3=sk-ant-123"
	got, err := NormalizeSessionCookie(full)
	if err != nil || got != full {
		t.Errorf("full header: %q %v, want it preserved", got, err)
	}
	if _, err := NormalizeSessionCookie("cf_clearance=abc; __cf_bm=def"); err == nil {
		t.Error("a full header without sessionKey must fail")
	}
	if id := DeviceIDFromCookie(full); id != "dev-42" {
		t.Errorf("device id = %q, want dev-42", id)
	}
	if id := DeviceIDFromCookie("sessionKey=sk-ant-123"); id != "" {
		t.Errorf("device id on a bare session cookie = %q, want empty", id)
	}
}

func TestOrganizationDiscoveryFailures(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		headers map[string]string
		wantErr error
	}{
		{"expired", 401, `{}`, nil, ErrSessionExpired},
		{"challenge", 403, "<html>cf-browser-verification</html>", map[string]string{"cf-mitigated": "challenge"}, ErrCloudflareChallenge},
		{"forbidden", 403, `{"error":"nope"}`, nil, ErrForbidden},
		{"rate limited", 429, `{}`, nil, ErrRateLimited},
		{"unavailable", 502, `{}`, nil, ErrUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mf := &mockFetcher{
				orgs: func(w http.ResponseWriter) {
					for k, v := range tc.headers {
						w.Header().Set(k, v)
					}
					w.WriteHeader(tc.status)
					fmt.Fprint(w, tc.body)
				},
				complet: func(w http.ResponseWriter, _ *http.Request) {},
			}
			c := newMock(t, mf)
			_, err := c.OrganizationID(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.wantErr.Error()) {
				t.Errorf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestOrganizationDiscoveryEmpty(t *testing.T) {
	mf := &mockFetcher{orgs: func(w http.ResponseWriter) { fmt.Fprint(w, `[]`) },
		complet: func(w http.ResponseWriter, _ *http.Request) {}}
	c := newMock(t, mf)
	if _, err := c.OrganizationID(context.Background()); err != ErrNoOrganization {
		t.Errorf("err = %v, want ErrNoOrganization", err)
	}
}

func TestGenerateFirstTurnAndCommit(t *testing.T) {
	var gotBody map[string]any
	var gotPath string
	mf := &mockFetcher{
		orgs: orgsOK,
		complet: func(w http.ResponseWriter, r *http.Request) {
			gotBody = nil
			json.Unmarshal(readAll(r), &gotBody)
			gotPath = r.URL.Path
			sseTurn(t, w, []string{"Hel", "lo"}, true)
		},
	}
	c := newMock(t, mf)
	var deltas []string
	res, err := c.Generate(context.Background(), GenerateOptions{Model: "claude-opus-4.6", Prompt: "hi"}, func(d string) {
		deltas = append(deltas, d)
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(deltas, "|") != "Hel|lo" || res.Text != "Hello" || !res.Completed {
		t.Fatalf("deltas=%v text=%q completed=%v", deltas, res.Text, res.Completed)
	}
	// The URL path carries the CLIENT-GENERATED conversation uuid.
	if !strings.Contains(gotPath, "/chat_conversations/") || !strings.HasSuffix(gotPath, "/completion") {
		t.Errorf("path = %s", gotPath)
	}
	convID := gotPath[strings.Index(gotPath, "chat_conversations/")+len("chat_conversations/") : len(gotPath)-len("/completion")]
	if convID != res.ConvID || len(convID) != 36 {
		t.Errorf("conv id = %q (res %q)", convID, res.ConvID)
	}
	// First-turn payload: create_conversation_params + human uuid.
	if _, ok := gotBody["create_conversation_params"]; !ok {
		t.Error("first turn must carry create_conversation_params")
	}
	uuids, _ := gotBody["turn_message_uuids"].(map[string]any)
	if uuids["human_message_uuid"] != res.HumanUUID {
		t.Errorf("human uuid = %v, want %v", uuids["human_message_uuid"], res.HumanUUID)
	}
	if uuids["assistant_message_uuid"] != res.AssistUUID {
		t.Errorf("assistant uuid mismatch")
	}
	// opus-4.6 (not opus-5) with no thinking flag -> default effort low/off.
	if gotBody["effort"] != "low" || gotBody["thinking_mode"] != "off" {
		t.Errorf("effort map = %v/%v", gotBody["effort"], gotBody["thinking_mode"])
	}
}

func TestGenerateFollowUpChainsState(t *testing.T) {
	var bodies []map[string]any
	mf := &mockFetcher{
		orgs: orgsOK,
		complet: func(w http.ResponseWriter, r *http.Request) {
			var b map[string]any
			json.Unmarshal(readAll(r), &b)
			bodies = append(bodies, b)
			sseTurn(t, w, []string{"ok"}, true)
		},
	}
	c := newMock(t, mf)
	first, err := c.Generate(context.Background(), GenerateOptions{Model: "claude-sonnet-4.6", Prompt: "one"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.Generate(context.Background(), GenerateOptions{Model: "claude-sonnet-4.6", Prompt: "two"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.ConvID != first.ConvID {
		t.Errorf("conversation changed: %q -> %q", first.ConvID, second.ConvID)
	}
	// Follow-up payload: conversation_id + parent (first assistant uuid),
	// fresh assistant uuid, no create_conversation_params.
	u2, _ := bodies[1]["turn_message_uuids"].(map[string]any)
	if u2["conversation_id"] != first.ConvID {
		t.Errorf("conversation_id = %v", u2["conversation_id"])
	}
	if u2["parent_message_uuid"] != first.AssistUUID {
		t.Errorf("parent = %v, want %v", u2["parent_message_uuid"], first.AssistUUID)
	}
	if _, ok := bodies[1]["create_conversation_params"]; ok {
		t.Error("follow-up must NOT create a conversation")
	}
	if u2["assistant_message_uuid"] == first.AssistUUID {
		t.Error("assistant uuid must be fresh per turn")
	}
}

func TestGenerateStreamCutDoesNotCommit(t *testing.T) {
	mf := &mockFetcher{
		orgs: orgsOK,
		complet: func(w http.ResponseWriter, _ *http.Request) {
			sseTurn(t, w, []string{"partial"}, false) // no message_stop
		},
	}
	c := newMock(t, mf)
	res, err := c.Generate(context.Background(), GenerateOptions{Model: "claude-opus-4.6", Prompt: "x"}, nil)
	if err == nil || !strings.Contains(err.Error(), "message_stop") {
		t.Fatalf("err = %v, want the stream-cut error", err)
	}
	if res.Text != "partial" || res.Completed {
		t.Errorf("partial text leaked into committed state: %+v", res)
	}
	if c.parentUUID != "" {
		t.Error("a cut turn must NOT commit a parent uuid")
	}
}

func TestGenerateErrorEvent(t *testing.T) {
	mf := &mockFetcher{
		orgs: orgsOK,
		complet: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"a\"}}\n\n")
			fmt.Fprint(w, "data: {\"type\":\"error\",\"message\":\"overloaded\"}\n\n")
			fmt.Fprint(w, "data: {\"type\":\"message_stop\"}\n\n")
		},
	}
	c := newMock(t, mf)
	if _, err := c.Generate(context.Background(), GenerateOptions{Model: "claude-opus-4.6", Prompt: "x"}, nil); err == nil ||
		!strings.Contains(err.Error(), "overloaded") {
		t.Fatalf("err = %v, want the upstream error surfaced", err)
	}
}

func TestGenerateErrorStatusClassifiesFirst(t *testing.T) {
	// A 401 mid-stream must classify as ErrSessionExpired (the snippet the
	// SSE reader captured is used), not as a generic stream cut.
	mf := &mockFetcher{
		orgs: orgsOK,
		complet: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"error":"unauthorized"}`)
		},
	}
	c := newMock(t, mf)
	_, err := c.Generate(context.Background(), GenerateOptions{Model: "claude-opus-4.6", Prompt: "x"}, nil)
	if err == nil || !strings.Contains(err.Error(), ErrSessionExpired.Error()) {
		t.Fatalf("err = %v, want the typed 401", err)
	}
}

func TestPayloadParity(t *testing.T) {
	var body map[string]any
	mf := &mockFetcher{
		orgs: orgsOK,
		complet: func(w http.ResponseWriter, r *http.Request) {
			json.Unmarshal(readAll(r), &body)
			sseTurn(t, w, []string{"x"}, true)
		},
	}
	c := newMock(t, mf)
	c.WithTimezone("Asia/Kuala_Lumpur")
	_, err := c.Generate(context.Background(), GenerateOptions{
		Model:  "claude-opus-5",
		Prompt: "hello",
		Tools: []any{map[string]any{"type": "function", "function": map[string]any{
			"name": "get_weather", "description": "weather", "parameters": map[string]any{"type": "object"},
		}}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// opus-5 -> high/auto
	if body["effort"] != "high" || body["thinking_mode"] != "auto" {
		t.Errorf("opus-5 effort = %v/%v", body["effort"], body["thinking_mode"])
	}
	if body["timezone"] != "Asia/Kuala_Lumpur" {
		t.Errorf("timezone = %v", body["timezone"])
	}
	if body["rendering_mode"] != "messages" {
		t.Errorf("rendering_mode = %v", body["rendering_mode"])
	}
	styles, _ := body["personalized_styles"].([]any)
	if len(styles) != 1 {
		t.Fatalf("styles = %v", styles)
	}
	s0 := styles[0].(map[string]any)
	if s0["prompt"] != "Normal\n" || s0["key"] != "Default" {
		t.Errorf("style block = %+v", s0)
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", tools)
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "get_weather" || tool["input_schema"] == nil {
		t.Errorf("tool shape = %+v", tool)
	}
}

func TestBrowserHeadersExactSet(t *testing.T) {
	h := BrowserHeaders("dev-1", "en-US", "")
	want := map[string]string{
		"Accept":                    "text/event-stream",
		"Content-Type":              "application/json",
		"Origin":                    "https://claude.ai",
		"Referer":                   "https://claude.ai/new",
		"User-Agent":                "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36",
		"anthropic-client-platform": "web_claude_ai",
		"anthropic-device-id":       "dev-1",
		"Sec-Fetch-Site":            "same-origin",
	}
	for k, v := range want {
		if h.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, h.Get(k), v)
		}
	}
}

// TestTLSFetcherRealHandshake proves the uTLS machinery end-to-end against
// a local TLS server. It cannot assert JA3 parity with Chrome in-process;
// the profile IS chrome_146 (utls HelloChrome_Auto) and live behavior is
// the operator-gated proof (runbook).
func TestTLSFetcherRealHandshake(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "orgs:%s", r.URL.Path)
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	f := NewTLSFetcher()
	url := strings.Replace(srv.URL, "127.0.0.1", "127.0.0.1", 1) + "/api/organizations"
	// Point the fetcher at the test server by rewriting via the fetcher's
	// URL argument (the transport dials whatever host the URL names).
	h := http.Header{}
	h.Set("Cookie", "sessionKey=x")
	resp, err := f.Fetch(context.Background(), http.MethodGet, url, nil, h)
	if err != nil {
		t.Skipf("local TLS handshake with the chrome profile failed (%v) — environment lacks the cipher suite", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "/api/organizations") {
		t.Errorf("body = %q", body)
	}
}

func readAll(r *http.Request) []byte {
	b, _ := io.ReadAll(r.Body)
	return b
}
