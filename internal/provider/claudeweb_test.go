package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newClaudeWebAdapterForTest is the test constructor: bridgeBase points the
// rewrite at a mock.
func newClaudeWebAdapterForTest(bridgeBase string) Adapter {
	return &claudeWebAdapter{client: &http.Client{Timeout: 5 * time.Second}, bridgeBase: strings.TrimSuffix(bridgeBase, "/")}
}

func TestClaudeWebAdapterShape(t *testing.T) {
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer bridge.Close()
	a := newClaudeWebAdapterForTest(bridge.URL + "/internal/claudeweb/v1")
	if a.Kind() != KindClaudeWeb {
		t.Fatalf("kind = %s", a.Kind())
	}
	if a.CatalogIsPublic() {
		t.Error("catalog must not be public")
	}
	if o := a.AuthOracle(); o.Method != http.MethodGet || o.Path != "/models" {
		t.Errorf("oracle = %+v", o)
	}
	if _, err := a.ReadQuota(context.Background(), Account{}, "k"); err != ErrQuotaUnsupported {
		t.Errorf("quota err = %v, want unsupported (no invented numbers)", err)
	}
	models, err := a.ListModels(context.Background(), Account{}, "sessionKey=x")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != len(ClaudeWebDefaultModels) {
		t.Errorf("models = %+v", models)
	}
	if models[0].OwnedBy != "anthropic" {
		t.Errorf("owned_by = %q", models[0].OwnedBy)
	}
	// The adapter stamps exactly the three loopback headers and nothing else.
	strip := a.StripHeaders()
	if len(strip) != 3 || strip[0] != ClaudeWebAccountHeader || strip[1] != ClaudeWebKeyHeader || strip[2] != BridgeSecretHeader {
		t.Errorf("strip = %v", strip)
	}
}

func TestClaudeWebPrepareRequestRewrite(t *testing.T) {
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer bridge.Close()
	a := newClaudeWebAdapterForTest(bridge.URL + "/internal/claudeweb/v1")
	req, _ := http.NewRequest(http.MethodPost, "internal://claudeweb/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer client-supplied")
	if err := a.PrepareRequest(context.Background(), req, Account{ID: 9}, "sessionKey=sk-ant-1", SurfaceChat); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(req.URL.String(), bridge.URL+"/internal/claudeweb/v1/chat/completions") {
		t.Errorf("rewritten = %s", req.URL)
	}
	if req.Header.Get("Authorization") != "" {
		t.Error("client Authorization must be dropped")
	}
	if req.Header.Get(ClaudeWebAccountHeader) != "9" {
		t.Errorf("account header = %q", req.Header.Get(ClaudeWebAccountHeader))
	}
	if !strings.Contains(req.Header.Get(ClaudeWebKeyHeader), "sk-ant-1") {
		t.Errorf("key header = %q", req.Header.Get(ClaudeWebKeyHeader))
	}
}

// Both web adapters stamp this process's bridge secret on every rewritten hop
// (the bridges refuse anything else) and strip any client-sent copy.
func TestWebAdaptersStampTheBridgeSecret(t *testing.T) {
	for name, ad := range map[string]Adapter{
		"claude-web": NewClaudeWeb("http://127.0.0.1:1/internal/claudeweb/v1", nil),
		"gemini-web": NewGeminiWeb("http://127.0.0.1:1/internal/geminiweb/v1", nil),
	} {
		req, _ := http.NewRequest("POST", "http://sentinel/chat/completions", nil)
		if err := ad.PrepareRequest(context.Background(), req, Account{ID: 3}, "k", SurfaceChat); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !ValidBridgeSecret(req.Header.Get(BridgeSecretHeader)) {
			t.Errorf("%s: hop does not carry the bridge secret", name)
		}
		found := false
		for _, h := range ad.StripHeaders() {
			found = found || h == BridgeSecretHeader
		}
		if !found {
			t.Errorf("%s: a client-sent %s would be forwarded", name, BridgeSecretHeader)
		}
	}
	if ValidBridgeSecret("") || ValidBridgeSecret(BridgeSecret()+"x") {
		t.Error("ValidBridgeSecret accepted a wrong value")
	}
}
