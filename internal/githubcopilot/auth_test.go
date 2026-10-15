package githubcopilot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Each mock stands in for one GitHub endpoint; the package URLs are consts,
// so tests route through an injected HTTP client that rewrites the host.
func routedClient(t *testing.T, routes map[string]http.HandlerFunc, reqs *[]string) *http.Client {
	t.Helper()
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		key := req.URL.Host + req.URL.Path
		if reqs != nil {
			*reqs = append(*reqs, key)
		}
		h, ok := routes[req.URL.Host]
		if !ok {
			return nil, fmt.Errorf("no mock route for host %s", req.URL.Host)
		}
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec.Result(), nil
	})}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// instantSleep is the poll wait for tests: no real time passes (the repo
// rule: synchronization-based tests, never sleeps).
func instantSleep(context.Context, time.Duration) error { return nil }

func jsonResp(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}
}

func TestSessionEncodeDecodeRoundTrip(t *testing.T) {
	s := Session{AccessToken: "gho_x", RefreshToken: "ghr_y", CopilotToken: "tid_z", CopilotExpiresAt: time.Now().Add(time.Hour).Truncate(time.Second)}
	got, err := DecodeSession(s.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != s.AccessToken || got.CopilotToken != s.CopilotToken || !got.CopilotExpiresAt.Equal(s.CopilotExpiresAt) {
		t.Fatalf("round trip = %+v", got)
	}
	// Legacy rows: a bare token string lifts into a session.
	legacy, err := DecodeSession("ghu_bare_token")
	if err != nil {
		t.Fatal(err)
	}
	if legacy.AccessToken != "ghu_bare_token" || legacy.CopilotToken != "" {
		t.Errorf("legacy = %+v", legacy)
	}
	if _, err := DecodeSession("   "); err == nil {
		t.Error("empty must fail")
	}
	if _, err := DecodeSession("{not json"); err == nil {
		t.Error("malformed must fail")
	}
}

func TestDeviceFlowEndToEnd(t *testing.T) {
	var polls int
	client := routedClient(t, map[string]http.HandlerFunc{
		"github.com": func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/login/device/code":
				r.ParseForm()
				if r.Form.Get("client_id") != defaultClientID {
					t.Errorf("client_id = %q, want the public Copilot CLI id", r.Form.Get("client_id"))
				}
				if r.Form.Get("scope") != "read:user" {
					t.Errorf("scope = %q, want read:user (the copilot scope routes to the restricted tier)", r.Form.Get("scope"))
				}
				jsonResp(200, `{"device_code":"dc-1","user_code":"ABCD-1234","verification_uri":"https://github.com/login/device","expires_in":600,"interval":0}`)(w, r)
			case "/login/oauth/access_token":
				r.ParseForm()
				if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" {
					t.Errorf("grant = %q", r.Form.Get("grant_type"))
				}
				polls++
				if polls < 2 {
					jsonResp(200, `{"error":"authorization_pending"}`)(w, r)
					return
				}
				jsonResp(200, `{"access_token":"gho_live","refresh_token":"ghr_live","token_type":"bearer"}`)(w, r)
			default:
				w.WriteHeader(404)
			}
		},
	}, nil)

	cfg := Config{HTTP: client, Scope: "read:user", Sleep: instantSleep}
	dc, err := RequestDeviceCode(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if dc.Interval != 5 {
		t.Errorf("interval default = %d, want 5", dc.Interval)
	}
	s, err := PollAccessToken(context.Background(), cfg, DeviceCode{
		DeviceCode: dc.DeviceCode, Interval: 0, ExpiresIn: 600,
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.AccessToken != "gho_live" || s.RefreshToken != "ghr_live" {
		t.Fatalf("session = %+v", s)
	}
}

func TestDeviceFlowDenied(t *testing.T) {
	client := routedClient(t, map[string]http.HandlerFunc{
		"github.com": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/login/device/code" {
				jsonResp(200, `{"device_code":"dc","user_code":"U","expires_in":600,"interval":1}`)(w, r)
				return
			}
			jsonResp(200, `{"error":"access_denied"}`)(w, r)
		},
	}, nil)
	cfg := Config{HTTP: client, Sleep: instantSleep}
	dc, _ := RequestDeviceCode(context.Background(), cfg)
	_, err := PollAccessToken(context.Background(), cfg, dc)
	if err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("err = %v", err)
	}
}

func TestMintCopilotToken(t *testing.T) {
	var gotAuth string
	client := routedClient(t, map[string]http.HandlerFunc{
		"api.github.com": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/copilot_internal/v2/token" {
				w.WriteHeader(404)
				return
			}
			gotAuth = r.Header.Get("Authorization")
			jsonResp(200, `{"token":"tid-123","expires_at":1893456000}`)(w, r)
		},
	}, nil)
	token, exp, err := MintCopilotToken(context.Background(), Config{HTTP: client}, "gho_x")
	if err != nil {
		t.Fatal(err)
	}
	// 'token <gh>' NOT Bearer — the official exchange.
	if gotAuth != "token gho_x" {
		t.Errorf("Authorization = %q, want 'token gho_x'", gotAuth)
	}
	if token != "tid-123" {
		t.Errorf("token = %q", token)
	}
	if want := time.Unix(1893456000, 0).UTC(); !exp.Equal(want) {
		t.Errorf("expires = %v, want %v", exp, want)
	}
	// 403 (ToS rejection for a bad credential) must surface, not silently pass.
	client403 := routedClient(t, map[string]http.HandlerFunc{
		"api.github.com": jsonResp(403, `{"message":"Bad credentials"}`),
	}, nil)
	if _, _, err := MintCopilotToken(context.Background(), Config{HTTP: client403}, "bad"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v, want the 403 surfaced", err)
	}
}

func TestEnsureSessionRefreshChain(t *testing.T) {
	minted := 0
	client := routedClient(t, map[string]http.HandlerFunc{
		"api.github.com": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/copilot_internal/v2/token" {
				minted++
				if minted == 1 {
					jsonResp(401, `{"message":"Bad credentials"}`)(w, r)
					return
				}
				jsonResp(200, `{"token":"tid-fresh","expires_at":1893456000}`)(w, r)
				return
			}
			w.WriteHeader(404)
		},
		"github.com": func(w http.ResponseWriter, r *http.Request) {
			r.ParseForm()
			if r.Form.Get("client_secret") != "" {
				t.Error("public client must NOT send client_secret (9router#442)")
			}
			jsonResp(200, `{"access_token":"gho_new","refresh_token":"ghr_new"}`)(w, r)
		},
	}, nil)
	cfg := Config{HTTP: client}
	s := Session{AccessToken: "gho_old", RefreshToken: "ghr_old"}
	got, err := EnsureSession(context.Background(), cfg, s, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "gho_new" || got.CopilotToken != "tid-fresh" {
		t.Fatalf("session = %+v", got)
	}
	if minted != 2 {
		t.Errorf("mint attempts = %d, want 2 (fail → refresh → retry)", minted)
	}
}

func TestEnsureSessionNoRefreshNeeded(t *testing.T) {
	s := Session{AccessToken: "gho", CopilotToken: "tid", CopilotExpiresAt: time.Now().Add(time.Hour)}
	got, err := EnsureSession(context.Background(), Config{HTTP: http.DefaultClient}, s, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got.CopilotToken != "tid" {
		t.Error("fresh token must pass through untouched")
	}
}

func TestEndpointRouting(t *testing.T) {
	cases := []struct{ model, target, want string }{
		{"gpt-4o", "", "https://api.githubcopilot.com/chat/completions"},
		{"claude-opus-4.6", "", "https://api.githubcopilot.com/v1/messages"},
		{"CLAUDE-SONNET-4.5", "", "https://api.githubcopilot.com/v1/messages"},
		{"gpt-5.3-codex", "", "https://api.githubcopilot.com/responses"},
		{"gpt-5.6-luna", "openai-responses", "https://api.githubcopilot.com/responses"},
		{"gemini-3.5-flash", "openai-responses", "https://api.githubcopilot.com/chat/completions"}, // never /responses
	}
	for _, tc := range cases {
		if got := endpointForModel(tc.model, tc.target); got != tc.want {
			t.Errorf("endpoint(%q,%q) = %s, want %s", tc.model, tc.target, got, tc.want)
		}
	}
}

func TestChatHeaders(t *testing.T) {
	h := ChatHeaders(Config{}, "tid-1", true)
	if h.Get("Authorization") != "Bearer tid-1" {
		t.Errorf("auth = %q", h.Get("Authorization"))
	}
	if h.Get("copilot-integration-id") != "copilot-developer-cli" {
		t.Errorf("integration = %q", h.Get("copilot-integration-id"))
	}
	if h.Get("copilot-harness-id") != "copilot-sdk" || h.Get("x-github-api-version") != "2026-08-01" {
		t.Errorf("headers = %v", h)
	}
	if !strings.Contains(h.Get("user-agent"), "copilot/1.0.81-6") {
		t.Errorf("ua = %q", h.Get("user-agent"))
	}
	if h.Get("x-stainless-helper-method") != "stream" {
		t.Error("stream marker missing on a streaming request")
	}
	// env-configurable CLI version (the 466 fix)
	h2 := ChatHeaders(Config{CLIVersion: "1.0.99"}, "t", false)
	if !strings.Contains(h2.Get("editor-version"), "copilot/1.0.99") {
		t.Errorf("version override = %q", h2.Get("editor-version"))
	}
	if h2.Get("x-stainless-helper-method") != "" {
		t.Error("stream marker must not appear on non-stream requests")
	}
}

func TestPrepareBodyQuirks(t *testing.T) {
	// gpt-5.4 temperature strip
	body := map[string]any{"temperature": 0.7, "messages": []any{}}
	PrepareBody("gpt-5.4", body)
	if _, ok := body["temperature"]; ok {
		t.Error("gpt-5.4 must strip temperature (400 upstream)")
	}
	// tools cap
	tools := make([]any, 200)
	body = map[string]any{"tools": tools}
	PrepareBody("gpt-4o", body)
	if got := body["tools"].([]any); len(got) != 128 {
		t.Errorf("tools = %d, want 128", len(got))
	}
	// assistant reasoning stripping
	body = map[string]any{"messages": []any{
		map[string]any{"role": "assistant", "reasoning_text": "hidden", "reasoning_content": "also", "content": "hi"},
		map[string]any{"role": "user", "content": "yo"},
	}}
	PrepareBody("gpt-4o", body)
	a := body["messages"].([]any)[0].(map[string]any)
	if _, ok := a["reasoning_text"]; ok {
		t.Error("reasoning_text must be stripped")
	}
	if _, ok := a["reasoning_content"]; ok {
		t.Error("reasoning_content must be stripped")
	}
	// trailing assistant prefill drop
	body = map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "q"},
		map[string]any{"role": "assistant", "content": "a"},
	}}
	PrepareBody("gpt-4o", body)
	if got := len(body["messages"].([]any)); got != 1 {
		t.Errorf("messages = %d, want the trailing assistant dropped", got)
	}
	// claude models keep their native shape (no sanitize, no drop)
	body = map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "content": "x"}}},
		map[string]any{"role": "assistant", "content": "a"},
	}}
	PrepareBody("claude-opus-4.6", body)
	if got := len(body["messages"].([]any)); got != 2 {
		t.Error("claude path must not drop the trailing assistant message")
	}
	// unknown parts serialize to text on the chat path
	body = map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_use", "text": "payload"}}},
	}}
	PrepareBody("gpt-4o", body)
	parts := body["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(parts) != 1 || parts[0].(map[string]any)["type"] != "text" {
		t.Errorf("sanitized parts = %+v", parts)
	}
}

func TestListModelsFiltersAndFallback(t *testing.T) {
	payload := `{"data":[
		{"id":"gpt-4o","name":"GPT-4o","model_picker_enabled":true,"policy":{"state":"enabled"},"capabilities":{"type":"chat"},"supported_endpoints":["/chat/completions"]},
		{"id":"gpt-4o-old","model_picker_enabled":false},
		{"id":"embed-model","model_picker_enabled":true,"policy":{"state":"enabled"},"capabilities":{"type":"embeddings"}},
		{"id":"disabled-one","model_picker_enabled":true,"policy":{"state":"disabled"}}
	]}`
	client := routedClient(t, map[string]http.HandlerFunc{
		"api.githubcopilot.com": jsonResp(200, payload),
	}, nil)
	models, err := ListModels(context.Background(), Config{HTTP: client}, "tid")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "gpt-4o" {
		t.Fatalf("models = %+v", models)
	}
	if len(models[0].SupportedEndpoints) != 1 || models[0].SupportedEndpoints[0] != "/chat/completions" {
		t.Errorf("endpoints = %v", models[0].SupportedEndpoints)
	}
	// 401 must fail (not silently fall back — fallback is for an empty
	// catalog with a live token, not for a dead credential).
	client2 := routedClient(t, map[string]http.HandlerFunc{
		"api.githubcopilot.com": jsonResp(401, `{}`),
	}, nil)
	if _, err := ListModels(context.Background(), Config{HTTP: client2}, "tid"); err == nil {
		t.Fatal("401 must fail, not fall back")
	}
	client3 := routedClient(t, map[string]http.HandlerFunc{
		"api.githubcopilot.com": jsonResp(200, `{"data":[]}`),
	}, nil)
	models3, err := ListModels(context.Background(), Config{HTTP: client3}, "tid")
	if err != nil {
		t.Fatal(err)
	}
	if len(models3) != len(fallbackModelIDs) {
		t.Errorf("fallback = %d models, want %d", len(models3), len(fallbackModelIDs))
	}
}

func TestNeedsRefresh(t *testing.T) {
	now := time.Now()
	if !NeedsRefresh(Session{}, now) {
		t.Error("no token must refresh")
	}
	if !NeedsRefresh(Session{CopilotToken: "t", CopilotExpiresAt: now.Add(4 * time.Minute)}, now) {
		t.Error("within 5 minutes must refresh")
	}
	if NeedsRefresh(Session{CopilotToken: "t", CopilotExpiresAt: now.Add(30 * time.Minute)}, now) {
		t.Error("fresh token must not refresh")
	}
}

func TestDoChatRoutingAndHeaders(t *testing.T) {
	var gotPath, gotAuth, gotVer string
	client := routedClient(t, map[string]http.HandlerFunc{
		"api.githubcopilot.com": func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			gotAuth = r.Header.Get("Authorization")
			gotVer = r.Header.Get("anthropic-version")
			jsonResp(200, `{"ok":true}`)(w, r)
		},
	}, nil)
	resp, err := DoChat(context.Background(), Config{HTTP: client},
		Session{CopilotToken: "tid-9"}, "claude-opus-4.6", "",
		map[string]any{"messages": []any{}}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if gotPath != "/v1/messages" {
		t.Errorf("path = %s", gotPath)
	}
	if gotAuth != "Bearer tid-9" {
		t.Errorf("auth = %q", gotAuth)
	}
	if gotVer != "2023-06-01" {
		t.Errorf("anthropic-version = %q (the /v1/messages shim requires it)", gotVer)
	}
}

func TestDeviceFlowCancel(t *testing.T) {
	client := routedClient(t, map[string]http.HandlerFunc{
		"github.com": jsonResp(200, `{"error":"authorization_pending"}`),
	}, nil)
	dc := DeviceCode{DeviceCode: "dc", Interval: 1, ExpiresIn: 600}
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	cfg := Config{HTTP: client, Sleep: func(c context.Context, _ time.Duration) error {
		calls++
		if calls >= 2 {
			cancel() // cancel from inside the poll wait
		}
		return c.Err()
	}}
	if _, err := PollAccessToken(ctx, cfg, dc); err == nil {
		t.Fatal("cancel must abort the poll")
	}
}

var _ = json.Marshal
