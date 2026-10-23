package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// The bridge test stands up a mock gemini.google.com (init page + status
// RPC + StreamGenerate frames) and a real temp sqlite DB with one enabled
// gemini-web account, then drives the bridge exactly like the adapter does.

func bridgeMockUpstream(t *testing.T, statusCode int, genFrames string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app":
			fmt.Fprint(w, `<html>{"SNlM0e":"tok-abc123","cfb2h":"boq","FdrFJe":"sid","TuX5cc":"en"}</html>`)
		case strings.Contains(r.URL.Path, "batchexecute"):
			body := make([]any, 18)
			body[14] = statusCode
			body[15] = []any{
				func() []any {
					d := make([]any, 18)
					d[0] = "abc123"
					d[10] = "Pro"
					d[11] = "Gemini 3 Pro"
					d[17] = 3
					return d
				}(),
			}
			bodyJSON, _ := json.Marshal(body)
			part, _ := json.Marshal([]any{"wrb.fr", "otAQ7b", string(bodyJSON), nil, nil, nil, nil, "generic"})
			units := 0
			for _, u := range string(part) {
				if u > 0xFFFF {
					units += 2
				} else {
					units++
				}
			}
			fmt.Fprintf(w, ")]}'\n%d\n%s", units+1, part)
		case strings.Contains(r.URL.Path, "StreamGenerate"):
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, genFrames)
		default:
			w.WriteHeader(404)
		}
	}))
}

func genFrame(text string, completed bool, final string) string {
	cand := make([]any, 9)
	cand[0] = "rc-1"
	cand[1] = []any{text}
	if completed {
		cand[8] = []any{2}
	}
	body := make([]any, 26)
	body[1] = []any{"cid-1", "rid-1", "rc-1"}
	body[4] = []any{cand}
	if final != "" {
		body[25] = final
	}
	bodyJSON, _ := json.Marshal(body)
	part, _ := json.Marshal([]any{"wrb.fr", "haW8u6", string(bodyJSON), nil, nil, nil, nil, "generic"})
	units := 0
	for _, u := range string(part) {
		if u > 0xFFFF {
			units += 2
		} else {
			units++
		}
	}
	return fmt.Sprintf("%d\n%s", units+1, part)
}

func newBridgeFixture(t *testing.T, genFrames string) (*geminiWebBridge, int64, *httptest.Server) {
	t.Helper()
	up := bridgeMockUpstream(t, 1000, genFrames)
	dir := t.TempDir()
	db, err := store.Open(context.Background(), store.Options{
		Path: filepath.Join(dir, "t.sqlite"), BatchSize: 4, BatchWait: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.UpsertAccount(context.Background(), provider.Account{
		Name: "gem", Namespace: "gem", Kind: provider.KindGeminiWeb,
		BaseURL: "internal://geminiweb", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.AddKey(context.Background(), id, "primary",
		"__Secure-1PSID=abc\n__Secure-1PSIDTS=def"); err != nil {
		t.Fatal(err)
	}
	b := newGeminiWebBridge(db, up.URL)
	t.Cleanup(up.Close)
	return b, id, up
}

func bridgeReq(t *testing.T, b *geminiWebBridge, method, path string, body string, acctID int64) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.Header.Set(provider.GeminiWebAccountHeader, fmt.Sprint(acctID))
	r.Header.Set(provider.BridgeSecretHeader, provider.BridgeSecret())
	w := httptest.NewRecorder()
	switch path {
	case geminiWebBridgePrefix + "/models":
		b.handleModels(w, r)
	default:
		b.handleChatCompletions(w, r)
	}
	return w
}

func TestBridgeModelsAuth(t *testing.T) {
	b, id, _ := newBridgeFixture(t, "")
	// no account header -> 401
	w := bridgeReq(t, b, "GET", geminiWebBridgePrefix+"/models", "", id)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d", w.Code)
	}
	var out struct {
		Data []struct{ ID string } `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != 1 || out.Data[0].ID != "gemini-pro" {
		t.Fatalf("models = %+v", out.Data)
	}

	// unknown account -> 401
	r := httptest.NewRequest("GET", geminiWebBridgePrefix+"/models", nil)
	r.Header.Set(provider.GeminiWebAccountHeader, "99999")
	r.Header.Set(provider.BridgeSecretHeader, provider.BridgeSecret())
	w2 := httptest.NewRecorder()
	b.handleModels(w2, r)
	if w2.Code != http.StatusUnauthorized {
		t.Errorf("unknown account code = %d", w2.Code)
	}

	// no account header at all (secret present, so the account check is what refuses) -> 401
	r3 := httptest.NewRequest("GET", geminiWebBridgePrefix+"/models", nil)
	r3.Header.Set(provider.BridgeSecretHeader, provider.BridgeSecret())
	w3 := httptest.NewRecorder()
	b.handleModels(w3, r3)
	if w3.Code != http.StatusUnauthorized {
		t.Errorf("missing header code = %d", w3.Code)
	}
}

func TestBridgeChatStream(t *testing.T) {
	frames := genFrame("Hel", false, "") + genFrame("Hello ", false, "") + genFrame("Hello world", true, "cid-1")
	b, id, _ := newBridgeFixture(t, frames)
	w := bridgeReq(t, b, "POST", geminiWebBridgePrefix+"/chat/completions",
		`{"model":"gemini-pro","messages":[{"role":"user","content":"hi"}],"stream":true}`, id)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "data: ") || !strings.Contains(body, "[DONE]") {
		t.Fatalf("not an SSE stream: %s", body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Errorf("missing stop finish: %s", body)
	}
	// The deltas must be the appended tails, not cumulative values.
	if !strings.Contains(body, `"content":"Hel"`) || !strings.Contains(body, `"content":"lo "`) ||
		!strings.Contains(body, `"content":"world"`) {
		t.Errorf("delta tails missing: %s", body)
	}
}

func TestBridgeChatNonStream(t *testing.T) {
	frames := genFrame("all at once", true, "cid-1")
	b, id, _ := newBridgeFixture(t, frames)
	w := bridgeReq(t, b, "POST", geminiWebBridgePrefix+"/chat/completions",
		`{"model":"gemini-pro","messages":[{"role":"user","content":"hi"}]}`, id)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d", w.Code)
	}
	var out struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Choices) != 1 || out.Choices[0].Message.Content != "all at once" {
		t.Fatalf("resp = %s", w.Body.String())
	}
}

func TestBridgeTypedRefusals(t *testing.T) {
	b, id, _ := newBridgeFixture(t, genFrame("x", true, "c"))
	for _, tc := range []struct{ name, body, want string }{
		{"tools", `{"model":"gemini-pro","messages":[{"role":"user","content":"x"}],"tools":[{"type":"function"}]}`, "unsupported_feature"},
		{"response_format", `{"model":"gemini-pro","messages":[{"role":"user","content":"x"}],"response_format":{"type":"json_object"}}`, "unsupported_feature"},
		{"unknown_model", `{"model":"gemini-99","messages":[{"role":"user","content":"x"}]}`, "model_not_found"},
	} {
		w := bridgeReq(t, b, "POST", geminiWebBridgePrefix+"/chat/completions", tc.body, id)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: code = %d", tc.name, w.Code)
		}
		if !strings.Contains(w.Body.String(), tc.want) {
			t.Errorf("%s: body = %s", tc.name, w.Body.String())
		}
	}
}

func TestBridgeUpstreamErrorMapping(t *testing.T) {
	// usage limit -> 429
	up := bridgeMockUpstream(t, 1000, errFramePart(1037))
	defer up.Close()
	dir := t.TempDir()
	db, err := store.Open(context.Background(), store.Options{Path: filepath.Join(dir, "t.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.UpsertAccount(context.Background(), provider.Account{
		Name: "gem", Namespace: "gem", Kind: provider.KindGeminiWeb,
		BaseURL: "internal://geminiweb", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	db.AddKey(context.Background(), id, "primary", "__Secure-1PSID=abc")
	b := newGeminiWebBridge(db, up.URL)
	w := bridgeReq(t, b, "POST", geminiWebBridgePrefix+"/chat/completions",
		`{"model":"gemini-pro","messages":[{"role":"user","content":"x"}]}`, id)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("usage limit code = %d body=%s", w.Code, w.Body.String())
	}
}

func TestBridgeDeadStatusModels(t *testing.T) {
	up := bridgeMockUpstream(t, 1016, "")
	defer up.Close()
	dir := t.TempDir()
	db, _ := store.Open(context.Background(), store.Options{Path: filepath.Join(dir, "t.sqlite")})
	id, _ := db.UpsertAccount(context.Background(), provider.Account{
		Name: "gem", Namespace: "gem", Kind: provider.KindGeminiWeb,
		BaseURL: "internal://geminiweb", Enabled: true,
	})
	db.AddKey(context.Background(), id, "primary", "__Secure-1PSID=abc")
	b := newGeminiWebBridge(db, up.URL)
	w := bridgeReq(t, b, "GET", geminiWebBridgePrefix+"/models", "", id)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("dead status code = %d, want 401", w.Code)
	}
}

// errFramePart builds a StreamGenerate frame carrying an upstream error code.
func errFramePart(code int) string {
	part := []any{
		"wrb.fr", "haW8u6", "null", nil, nil,
		[]any{nil, nil, []any{[]any{nil, []any{code}}}},
		"generic",
	}
	b, _ := json.Marshal(part)
	units := 0
	for _, u := range string(b) {
		if u > 0xFFFF {
			units += 2
		} else {
			units++
		}
	}
	return fmt.Sprintf("%d\n%s", units+1, b)
}

func TestBridgeRotatorPersists(t *testing.T) {
	// The rotator: two clientFor calls past the interval persist a rotated
	// cookie. The mock upstream's RotateCookies is not exercised here (it is
	// covered in the geminiweb package); this asserts the bridge keeps its
	// client cached and rotates on schedule without losing the session.
	b, id, _ := newBridgeFixture(t, genFrame("x", true, "c"))
	b.now = func() time.Time { return time.Unix(0, 0).Add(20 * time.Minute) }
	entry, err := b.clientFor(context.Background(), id, "")
	if err != nil {
		t.Fatal(err)
	}
	first := entry.client
	entry2, err := b.clientFor(context.Background(), id, "")
	if err != nil {
		t.Fatal(err)
	}
	if entry2.client != first {
		t.Error("client must be cached across calls")
	}
	if _, err := b.models(context.Background(), entry2); err != nil {
		t.Fatal(err)
	}
	// keyID wiring: PickKey filled it, so rotation has something to persist.
	if entry2.keyID == 0 {
		t.Error("keyID should be the picked provider_keys row")
	}
}

func TestBridgeRotateRevertsOnBadInit(t *testing.T) {
	// Stateful mock: /app yields SNlM0e only before rotation; after
	// RotateCookies is hit, /app stops yielding it. maybeRotate must
	// revert to the pre-rotation session instead of stranding it.
	var rotated bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app":
			if rotated {
				fmt.Fprint(w, `<html>no token here</html>`)
				return
			}
			fmt.Fprint(w, `<html>{"SNlM0e":"tok-abc123","cfb2h":"boq","FdrFJe":"sid","TuX5cc":"en"}</html>`)
		case strings.Contains(r.URL.Path, "RotateCookies"):
			rotated = true
			http.SetCookie(w, &http.Cookie{Name: "__Secure-1PSIDTS", Value: "fresh-ts"})
			fmt.Fprint(w, "[]")
		case strings.Contains(r.URL.Path, "batchexecute"):
			body := make([]any, 18)
			body[14] = 1000
			body[15] = []any{func() []any {
				d := make([]any, 18)
				d[0] = "abc123"
				d[10] = "Pro"
				d[11] = "Gemini 3 Pro"
				d[17] = 3
				return d
			}()}
			bodyJSON, _ := json.Marshal(body)
			part, _ := json.Marshal([]any{"wrb.fr", "otAQ7b", string(bodyJSON), nil, nil, nil, nil, "generic"})
			units := 0
			for _, u := range string(part) {
				if u > 0xFFFF {
					units += 2
				} else {
					units++
				}
			}
			fmt.Fprintf(w, ")]}'\n%d\n%s", units+1, part)
		case strings.Contains(r.URL.Path, "StreamGenerate"):
			fmt.Fprint(w, genFrame("ok", true, "c"))
		default:
			w.WriteHeader(404)
		}
	}))
	defer up.Close()

	dir := t.TempDir()
	db, err := store.Open(context.Background(), store.Options{Path: filepath.Join(dir, "t.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.UpsertAccount(context.Background(), provider.Account{
		Name: "gem", Namespace: "gem", Kind: provider.KindGeminiWeb,
		BaseURL: "internal://geminiweb", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	db.AddKey(context.Background(), id, "primary", "__Secure-1PSID=abc\n__Secure-1PSIDTS=def")
	b := newGeminiWebBridge(db, up.URL)
	// Force a rotation on the first call.
	b.now = func() time.Time { return time.Unix(0, 0).Add(20 * time.Minute) }

	// First call rotates (and re-init fails) -> must revert, not strand.
	w := bridgeReq(t, b, "POST", geminiWebBridgePrefix+"/chat/completions",
		`{"model":"gemini-pro","messages":[{"role":"user","content":"hi"}]}`, id)
	if w.Code != http.StatusOK {
		t.Fatalf("first call code = %d body=%s", w.Code, w.Body.String())
	}
	// Second call must still work (old session intact).
	w2 := bridgeReq(t, b, "POST", geminiWebBridgePrefix+"/chat/completions",
		`{"model":"gemini-pro","messages":[{"role":"user","content":"hi2"}]}`, id)
	if w2.Code != http.StatusOK {
		t.Fatalf("second call code = %d body=%s", w2.Code, w2.Body.String())
	}
}

// TestBridgeResolveVersusReinitRace pins C1 from the 2026-10-23 audit:
// resolve read the cached client's access token under the bridge map mutex
// while rotation re-ran Init under the per-account lock, so the two had no
// common lock. Meaningful under -race: the detector flags the old ordering.
func TestBridgeResolveVersusReinitRace(t *testing.T) {
	b, id, _ := newBridgeFixture(t, genFrame("x", true, "c"))
	if w := bridgeReq(t, b, "GET", geminiWebBridgePrefix+"/models", "", id); w.Code != http.StatusOK {
		t.Fatalf("warm-up status = %d body=%s", w.Code, w.Body)
	}
	b.mu.Lock()
	entry := b.clients[id]
	b.mu.Unlock()

	const n = 20
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < n; i++ {
			lock := b.lockFor(id) // rotation's lock (maybeRotate runs under it)
			lock.Lock()
			_ = entry.client.Init(context.Background())
			lock.Unlock()
		}
	}()
	for i := 0; i < n; i++ {
		if w := bridgeReq(t, b, "GET", geminiWebBridgePrefix+"/models", "", id); w.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", w.Code, w.Body)
		}
	}
	<-done
}
