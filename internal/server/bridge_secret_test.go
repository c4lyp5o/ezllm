package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// countHits wraps a mock upstream so a test can prove it was never called.
func countHits(up *httptest.Server) *atomic.Int64 {
	var n atomic.Int64
	inner := up.Config.Handler
	up.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		inner.ServeHTTP(w, r)
	})
	return &n
}

// The web bridges share the public listener, and a bare account id would
// unlock that account's STORED cookie. Without this process's bridge secret
// the request must be refused before the stored credential is ever used.
func TestWebBridgesRefuseWithoutTheProcessSecret(t *testing.T) {
	type call func(secret string, chat bool) int
	cw, cwID, cwUp := claudeWebFixture(t, &claudeWebMock{complet: func(w http.ResponseWriter, _ *http.Request) { claudeWebSSE(w, "hi") }})
	gw, gwID, gwUp := newBridgeFixture(t, genFrame("hi", true, "cid"))
	cwHits, gwHits := countHits(cwUp), countHits(gwUp)

	bridges := map[string]call{
		"claude-web": func(secret string, chat bool) int {
			return bridgeCall(t, secret, chat, provider.ClaudeWebAccountHeader, cwID, claudeWebBridgePrefix, cw.handleModels, cw.handleChatCompletions)
		},
		"gemini-web": func(secret string, chat bool) int {
			return bridgeCall(t, secret, chat, provider.GeminiWebAccountHeader, gwID, geminiWebBridgePrefix, gw.handleModels, gw.handleChatCompletions)
		},
	}
	for name, do := range bridges {
		for _, secret := range []string{"", "not-the-secret", strings.Repeat("0", len(provider.BridgeSecret()))} {
			for _, chat := range []bool{false, true} {
				if code := do(secret, chat); code != http.StatusUnauthorized {
					t.Errorf("%s chat=%v secret=%q: status %d, want 401", name, chat, secret, code)
				}
			}
		}
	}
	if cwHits.Load() != 0 || gwHits.Load() != 0 {
		t.Fatalf("stored credential was used without the secret: claude.ai hits %d, gemini hits %d", cwHits.Load(), gwHits.Load())
	}

	// The adapter's own stamp is admitted.
	if code := bridges["claude-web"](provider.BridgeSecret(), false); code != http.StatusOK {
		t.Errorf("claude-web with the secret: status %d, want 200", code)
	}
	if code := bridges["gemini-web"](provider.BridgeSecret(), false); code != http.StatusOK {
		t.Errorf("gemini-web with the secret: status %d, want 200", code)
	}
}

func bridgeCall(t *testing.T, secret string, chat bool, acctHeader string, acctID int64, prefix string, models, completions http.HandlerFunc) int {
	t.Helper()
	path, body, h := prefix+"/models", "", models
	if chat {
		path, body, h = prefix+"/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, completions
	}
	r := httptest.NewRequest(map[bool]string{false: "GET", true: "POST"}[chat], path, strings.NewReader(body))
	r.Header.Set(acctHeader, fmt.Sprint(acctID))
	if secret != "" {
		r.Header.Set(provider.BridgeSecretHeader, secret)
	}
	w := httptest.NewRecorder()
	h(w, r)
	return w.Code
}

// A cached claude-web session is reused only for the cookie it was built
// from: a replaced or rotated key gets a fresh client, never the old session.
func TestClaudeWebClientCacheFollowsTheCookie(t *testing.T) {
	b := newClaudeWebBridge(nil, nil)
	acct := &provider.Account{ID: 7, Kind: provider.KindClaudeWeb}
	get := func(key string) any {
		t.Helper()
		c, err := b.clientFor(context.Background(), acct, key)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	a1, a2 := get("sessionKey=sk-ant-a"), get("sessionKey=sk-ant-a")
	if a1 != a2 {
		t.Error("same cookie should reuse the cached session")
	}
	if bc := get("sessionKey=sk-ant-b"); bc == a1 {
		t.Error("a different cookie reused the old session")
	}
}
