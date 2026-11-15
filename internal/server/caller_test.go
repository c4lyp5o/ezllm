package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/codexbridge"
	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/store"
)

type securityCodexExecutor struct{ callers chan string }

func (f *securityCodexExecutor) Run(_ context.Context, r codexbridge.Request, _ func(codexbridge.Delta) error) (codexbridge.Result, error) {
	f.callers <- r.CallerID
	return codexbridge.Result{Text: "ok", Usage: codexbridge.Usage{InputTokens: 10, OutputTokens: 2}}, nil
}
func (*securityCodexExecutor) Models(context.Context) ([]codexbridge.Model, error) {
	return []codexbridge.Model{{ID: "gpt-6-luna"}}, nil
}
func (*securityCodexExecutor) RateLimits(context.Context) (codexbridge.RateLimits, error) {
	return codexbridge.RateLimits{}, nil
}

func TestSecurityRegressionGatewayAuthenticatesCodexCallerEndToEnd(t *testing.T) {
	f := &securityCodexExecutor{callers: make(chan string, 2)}
	native := (&codexbridge.Server{Runner: f, Token: "sk-upstream-key-123456", RequireCaller: true}).Handler()
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { native.ServeHTTP(w, r) })
	if _, err := h.db.UpsertAccount(context.Background(), provider.Account{Name: "up", Namespace: "up", Kind: provider.KindOpenAICompatible, BaseURL: h.upstream.URL + "/v1", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	h.server.EnableBridge("chatgpt-sub", strings.TrimPrefix(h.upstream.URL, "http://"), "sk-upstream-key-123456", "", nil)
	body := `{"model":"up/gpt-6-luna","caller_id":"forged-json","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"get_weather"}}]}`
	for _, token := range []string{"tok-infer", "tok-admin"} {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(provider.BridgeCallerHeader, "forged-header")
		response := httptest.NewRecorder()
		h.handler.ServeHTTP(response, req)
		if response.Code != 200 {
			t.Fatalf("gateway-to-bridge status %d: %s", response.Code, response.Body.String())
		}
		if id := <-f.callers; id != "client:"+store.HashToken(token) {
			t.Fatal("unverified caller identity reached native executor")
		}
	}
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set(provider.BridgeCallerHeader, "forged-header")
	response := httptest.NewRecorder()
	h.handler.ServeHTTP(response, req)
	if response.Code != 401 || len(f.callers) != 0 {
		t.Fatal("caller header bypassed gateway authentication")
	}
}
