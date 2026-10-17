package server

// Dashboard GitHub device flow (the phone path): start a flow, poll it to
// authorization, then store the credential through the NORMAL key-add route
// with device_handle — no terminal, no session JSON in a browser or a log.
//
// Every GitHub endpoint is faked through a routed HTTP client (the technique
// internal/githubcopilot uses): device code, token poll, mint, identity,
// catalog and chat all answer in-process, so no test dials github.com.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/githubcopilot"
	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/registration"
)

// fakeGitHub stands in for GitHub's four endpoints. authorized flips the
// moment the human "taps Authorize"; everything else is fixed truth.
type fakeGitHub struct {
	authorized atomic.Bool
	slowDown   atomic.Bool  // RFC 8628 §3.5 — GitHub says "poll less often"
	models     atomic.Int64 // /models hits, so the catalog step is provable
}

type ghRoundTripFunc func(*http.Request) (*http.Response, error)

func (f ghRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// client routes GitHub hosts to the fake and everything else (the harness's
// own httptest upstream) to the real transport.
func (f *fakeGitHub) client() *http.Client {
	return &http.Client{Transport: ghRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "github.com" && req.URL.Host != "api.github.com" && req.URL.Host != "api.githubcopilot.com" {
			return http.DefaultTransport.RoundTrip(req)
		}
		rec := httptest.NewRecorder()
		f.serve(rec, req)
		return rec.Result(), nil
	})}
}

func (f *fakeGitHub) serve(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch req.URL.Host {
	case "github.com":
		switch req.URL.Path {
		case "/login/device/code":
			fmt.Fprint(w, `{"device_code":"dc-test","user_code":"TEST-CODE","verification_uri":"https://github.com/login/device","expires_in":900,"interval":5}`)
		case "/login/oauth/access_token":
			// slow_down must be answered before authorization_pending: GitHub
			// sends it as its own verdict and withholds the token until the
			// poller slows down, so an accurate fake answers the same way.
			if f.slowDown.Load() {
				fmt.Fprint(w, `{"error":"slow_down"}`)
			} else if f.authorized.Load() {
				fmt.Fprint(w, `{"access_token":"gho_dev_session","refresh_token":"ghr_dev_session","token_type":"bearer"}`)
			} else {
				fmt.Fprint(w, `{"error":"authorization_pending"}`)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	case "api.github.com":
		switch req.URL.Path {
		case "/copilot_internal/v2/token":
			fmt.Fprintf(w, `{"token":"cop_test_token","expires_at":%d}`, time.Now().Add(30*time.Minute).Unix())
		case "/user":
			fmt.Fprint(w, `{"id":1,"login":"calypso"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	case "api.githubcopilot.com":
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/models":
			f.models.Add(1)
			if auth := req.Header.Get("Authorization"); auth == "" {
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, `{"error":{"message":"Bad credentials","type":"invalid_request_error"}}`)
				return
			}
			fmt.Fprint(w, `{"data":[{"id":"gpt-5.6-luna","name":"Luna","capabilities":{"type":"chat"},"policy":{"state":"enabled"},"model_picker_enabled":true}]}`)
		case req.Method == http.MethodPost && req.URL.Path == "/chat/completions":
			fmt.Fprint(w, `{"id":"c1","object":"chat.completion","model":"gpt-5.6-luna","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":10,"total_tokens":17,"prompt_tokens_details":{"cached_tokens":2}}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// githubHarness is newHarness with the device flow AND the key test's
// registry pointed at the fake: one routed client serves both, so the whole
// authorize-then-store path runs without a network call.
func githubHarness(t *testing.T, f *fakeGitHub) *harness {
	t.Helper()
	routed := f.client()
	return newHarness(t, (&fakeUpstream{}).handler(), func(o *Options) {
		o.GitHubDevice = githubcopilot.Config{HTTP: routed}
		o.Registry = provider.NewRegistry(routed)
		o.Tester = registration.NewTester(routed, o.Registry, o.DB)
	})
}

// startGHFlow begins a flow and returns its handle.
func startGHFlow(t *testing.T, h *harness) string {
	t.Helper()
	code, resp := jsonDo(t, h, "POST", "/admin/github/device", "tok-admin", nil)
	if code != http.StatusOK {
		t.Fatalf("start device flow = %d %s", code, resp)
	}
	out := mustJSON(t, resp)
	if out["user_code"] == "" || out["handle"] == "" || out["verification_uri"] == "" {
		t.Fatalf("start response missing flow fields: %s", resp)
	}
	return out["handle"].(string)
}

func ghStatus(t *testing.T, h *harness, handle string) map[string]any {
	t.Helper()
	code, resp := jsonDo(t, h, "GET", "/admin/github/device/"+handle, "tok-admin", nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d %s", code, resp)
	}
	return mustJSON(t, resp)
}

// TestGitHubDeviceFlowStoresTheKey is the whole phone path end to end:
// start → pending → authorize → authorized (login visible, session NOT) →
// account + key add by device_handle → handle retired.
func TestGitHubDeviceFlowStoresTheKey(t *testing.T) {
	f := &fakeGitHub{}
	h := githubHarness(t, f)

	handle := startGHFlow(t, h)

	// Not authorized yet: pending, and nothing resembling a credential.
	if st := ghStatus(t, h, handle); st["status"] != "pending" {
		t.Fatalf("pre-auth status = %v, want pending", st)
	}

	f.authorized.Store(true)
	st := ghStatus(t, h, handle)
	if st["status"] != "authorized" || st["github_login"] != "calypso" {
		t.Fatalf("post-auth status = %v, want authorized as calypso", st)
	}
	if _, body := jsonDo(t, h, "GET", "/admin/github/device/"+handle, "tok-admin", nil); strings.Contains(body, "gho_dev") || strings.Contains(body, "access_token") {
		t.Fatalf("status leaked session material: %s", body)
	}

	// Account, exactly as the add-provider modal creates it.
	code, resp := jsonDo(t, h, "POST", "/admin/accounts", "tok-admin", map[string]any{
		"name": "GitHub Copilot", "namespace": "github-copilot", "kind": "github-copilot",
		"base_url": "https://api.githubcopilot.com",
	})
	if code != http.StatusCreated {
		t.Fatalf("create account = %d %s", code, resp)
	}
	accountID := int(mustJSON(t, resp)["id"].(float64))

	// The key add names the handle; the server resolves the session itself.
	code, resp = jsonDo(t, h, "POST", fmt.Sprintf("/admin/accounts/%d/keys", accountID), "tok-admin",
		map[string]any{"label": "primary", "device_handle": handle})
	if code != http.StatusCreated {
		t.Fatalf("key add by device_handle = %d %s, want 201", code, resp)
	}
	result := mustJSON(t, resp)
	test, _ := result["test"].(map[string]any)
	if test == nil || test["ok"] != true {
		t.Fatalf("key test did not pass: %s", resp)
	}

	// Stored row: hint only, no session material anywhere in the response.
	if _, resp = jsonDo(t, h, "GET", fmt.Sprintf("/admin/accounts/%d/keys", accountID), "tok-admin", nil); strings.Contains(resp, "gho_dev") {
		t.Fatalf("key list leaked the session: %s", resp)
	}
	if got := countTable(t, h, "provider_keys"); got != 2 {
		t.Fatalf("provider_keys = %d, want 2 (seed + device flow)", got)
	}
	// The catalog came from the fake's /models, through the minted token.
	if f.models.Load() == 0 {
		t.Fatal("catalog step never called /models")
	}

	// Consumed: the handle's copy must be dead now that the row exists.
	code, resp = jsonDo(t, h, "POST", fmt.Sprintf("/admin/accounts/%d/keys", accountID), "tok-admin",
		map[string]any{"label": "second", "device_handle": handle})
	if code == http.StatusCreated {
		t.Fatalf("consumed handle reused: %d %s", code, resp)
	}
}

// TestGitHubDeviceFlowRefusals pins the guard rails: an unknown handle is a
// 404 on both routes, and an un-authorized one cannot store a key.
func TestGitHubDeviceFlowRefusals(t *testing.T) {
	f := &fakeGitHub{}
	h := githubHarness(t, f)

	if code, resp := jsonDo(t, h, "GET", "/admin/github/device/deadbeef", "tok-admin", nil); code != http.StatusNotFound {
		t.Fatalf("unknown handle status = %d %s", code, resp)
	}
	code, resp := jsonDo(t, h, "POST", "/admin/accounts", "tok-admin", map[string]any{
		"name": "GitHub Copilot", "namespace": "github-copilot", "kind": "github-copilot",
		"base_url": "https://api.githubcopilot.com",
	})
	if code != http.StatusCreated {
		t.Fatalf("create account = %d %s", code, resp)
	}
	accountID := int(mustJSON(t, resp)["id"].(float64))

	// Pending handle: refuse (409), store nothing.
	handle := startGHFlow(t, h)
	code, resp = jsonDo(t, h, "POST", fmt.Sprintf("/admin/accounts/%d/keys", accountID), "tok-admin",
		map[string]any{"label": "primary", "device_handle": handle})
	if code != http.StatusConflict {
		t.Fatalf("pending handle key add = %d %s, want 409", code, resp)
	}
	// Unknown handle: 404, store nothing.
	code, resp = jsonDo(t, h, "POST", fmt.Sprintf("/admin/accounts/%d/keys", accountID), "tok-admin",
		map[string]any{"label": "primary", "device_handle": "deadbeef"})
	if code != http.StatusNotFound {
		t.Fatalf("unknown handle key add = %d %s, want 404", code, resp)
	}
	if n := countTable(t, h, "provider_keys"); n != 1 {
		t.Fatalf("provider_keys = %d, want only the seed row", n)
	}

	// Both credential sources at once is a client bug, not a guess.
	if code, resp = jsonDo(t, h, "POST", fmt.Sprintf("/admin/accounts/%d/keys", accountID), "tok-admin",
		map[string]any{"label": "x", "api_key": "sk-…", "device_handle": handle}); code != http.StatusBadRequest {
		t.Fatalf("api_key + device_handle = %d %s, want 400", code, resp)
	}

	// Cancel drops the flow: the handle 404s afterwards.
	if code, resp = jsonDo(t, h, "DELETE", "/admin/github/device/"+handle, "tok-admin", nil); code != http.StatusNoContent {
		t.Fatalf("cancel = %d %s, want 204", code, resp)
	}
	if code, resp = jsonDo(t, h, "GET", "/admin/github/device/"+handle, "tok-admin", nil); code != http.StatusNotFound {
		t.Fatalf("cancelled handle status = %d %s, want 404", code, resp)
	}
}
