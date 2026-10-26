package server

// Subscription bridges in the dashboard (bridges.go): the Add Provider dialog
// lists both bridges with their state, a key add can name `bridge` so the
// server supplies the shared secret, and a test against a bridge that is not
// running names the variable that fixes it. The secret never appears in a
// response.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const bridgeSecret = "bridge-secret-5f2c9a"

func TestBridgesListsBothOffByDefault(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	code, resp := jsonDo(t, h, "GET", "/admin/bridges", "tok-admin", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /admin/bridges = %d %s", code, resp)
	}
	for _, want := range []string{`"id":"claude-sub"`, `"id":"chatgpt-sub"`, `"EZLLM_CLAUDE_BRIDGE"`, `"EZLLM_CODEX_BRIDGE"`, `"id":"antigravity-sub"`, `"EZLLM_AGY_BRIDGE"`, `"enabled":false`, `"label":"Claude Pro/Max"`, `"label":"ChatGPT Plus/Pro"`, `"label":"Google Antigravity"`} {
		if !strings.Contains(resp, want) {
			t.Fatalf("bridges response lacks %s: %s", want, resp)
		}
	}
	if strings.Contains(resp, `"enabled":true`) {
		t.Fatalf("a bridge reads as running before main enabled it: %s", resp)
	}
	if code, _ := jsonDo(t, h, "GET", "/admin/bridges", "tok-infer", nil); code != http.StatusForbidden && code != http.StatusUnauthorized {
		t.Fatalf("inference token read /admin/bridges: %d", code)
	}
}

// bridgeUpstream is a fake bridge that only answers the shared secret.
func bridgeUpstream(authed *atomic.Int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+bridgeSecret {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":{"message":"bad bridge token","type":"invalid_request_error"}}`)
			return
		}
		authed.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			fmt.Fprint(w, `{"object":"list","data":[{"id":"gpt-6-luna","object":"model"}]}`)
		case "/v1/chat/completions":
			fmt.Fprint(w, okChat)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func createBridgeAccount(t *testing.T, h *harness, baseURL string) int {
	t.Helper()
	code, resp := jsonDo(t, h, "POST", "/admin/accounts", "tok-admin", map[string]any{
		"name": "ChatGPT sub", "namespace": "chatgpt-sub", "kind": "openai-compatible", "base_url": baseURL,
	})
	if code != http.StatusCreated {
		t.Fatalf("create account = %d %s", code, resp)
	}
	return int(mustJSON(t, resp)["id"].(float64))
}

func TestBridgeKeyAddUsesTheServerSecret(t *testing.T) {
	var authed atomic.Int64
	h := newHarness(t, bridgeUpstream(&authed))
	addr := strings.TrimPrefix(h.upstream.URL, "http://")
	h.server.EnableBridge("chatgpt-sub", addr, bridgeSecret, map[string]any{"login": "logged-in"})

	_, list := jsonDo(t, h, "GET", "/admin/bridges", "tok-admin", nil)
	if !strings.Contains(list, `"enabled":true`) || !strings.Contains(list, `"login":"logged-in"`) {
		t.Fatalf("running bridge not reported: %s", list)
	}
	if strings.Contains(list, bridgeSecret) {
		t.Fatalf("GET /admin/bridges leaked the shared secret: %s", list)
	}

	id := createBridgeAccount(t, h, h.upstream.URL+"/v1")
	code, resp := jsonDo(t, h, "POST", fmt.Sprintf("/admin/accounts/%d/keys", id), "tok-admin",
		map[string]any{"label": "primary", "bridge": "chatgpt-sub"})
	if code != http.StatusCreated {
		t.Fatalf("key add by bridge = %d %s, want 201", code, resp)
	}
	if authed.Load() == 0 {
		t.Fatal("the key test never reached the bridge with its secret")
	}
	if strings.Contains(resp, bridgeSecret) {
		t.Fatalf("key add response leaked the secret: %s", resp)
	}
}

func TestBridgeKeyAddRefusals(t *testing.T) {
	var authed atomic.Int64
	h := newHarness(t, bridgeUpstream(&authed))
	id := createBridgeAccount(t, h, h.upstream.URL+"/v1")
	keys := fmt.Sprintf("/admin/accounts/%d/keys", id)

	// Off: refused, and the refusal says how to turn it on.
	code, resp := jsonDo(t, h, "POST", keys, "tok-admin", map[string]any{"bridge": "chatgpt-sub"})
	if code != http.StatusConflict || !strings.Contains(resp, "EZLLM_CODEX_BRIDGE=on") {
		t.Fatalf("bridge off = %d %s, want 409 naming EZLLM_CODEX_BRIDGE=on", code, resp)
	}

	h.server.EnableBridge("chatgpt-sub", strings.TrimPrefix(h.upstream.URL, "http://"), bridgeSecret, nil)

	// The secret only goes to the bridge that minted it.
	other := createBridgeAccount2(t, h, "https://api.example.com/v1")
	code, resp = jsonDo(t, h, "POST", fmt.Sprintf("/admin/accounts/%d/keys", other), "tok-admin", map[string]any{"bridge": "chatgpt-sub"})
	if code != http.StatusBadRequest {
		t.Fatalf("bridge secret for a foreign base URL = %d %s, want 400", code, resp)
	}
	// One credential source per add.
	code, resp = jsonDo(t, h, "POST", keys, "tok-admin", map[string]any{"bridge": "chatgpt-sub", "api_key": "sk-x"})
	if code != http.StatusBadRequest {
		t.Fatalf("bridge + api_key = %d %s, want 400", code, resp)
	}
	code, resp = jsonDo(t, h, "POST", keys, "tok-admin", map[string]any{"bridge": "nope"})
	if code != http.StatusBadRequest {
		t.Fatalf("unknown bridge = %d %s, want 400", code, resp)
	}
	if authed.Load() != 0 {
		t.Fatal("a refused add still reached the bridge")
	}
}

func createBridgeAccount2(t *testing.T, h *harness, baseURL string) int {
	t.Helper()
	code, resp := jsonDo(t, h, "POST", "/admin/accounts", "tok-admin", map[string]any{
		"name": "Elsewhere", "namespace": "elsewhere", "kind": "openai-compatible", "base_url": baseURL,
	})
	if code != http.StatusCreated {
		t.Fatalf("create account = %d %s", code, resp)
	}
	return int(mustJSON(t, resp)["id"].(float64))
}

// A pasted key against a bridge that is not running fails with a connection
// error; the failure must say which variable turns the bridge on.
func TestBridgeOffKeyTestNamesTheFix(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL + "/v1"
	dead.Close() // a port nothing listens on, like a bridge that is off
	h.server.bridges.list[1].BaseURL = deadURL

	id := createBridgeAccount(t, h, deadURL)
	code, resp := jsonDo(t, h, "POST", fmt.Sprintf("/admin/accounts/%d/keys", id), "tok-admin",
		map[string]any{"api_key": "pasted-token"})
	if code < 400 {
		t.Fatalf("key test against a dead bridge = %d %s, want a failure", code, resp)
	}
	if !strings.Contains(resp, "EZLLM_CODEX_BRIDGE=on") || !strings.Contains(resp, "codex-bridge-runbook") {
		t.Fatalf("failure does not name the fix: %s", resp)
	}
}
