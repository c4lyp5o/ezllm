package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
	// The adapter stamps exactly the two loopback headers and nothing else.
	strip := a.StripHeaders()
	if len(strip) != 2 || strip[0] != ClaudeWebAccountHeader || strip[1] != ClaudeWebKeyHeader {
		t.Errorf("strip = %v", strip)
	}
}

func TestClaudeWebPrepareRequestRewrite(t *testing.T) {
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer bridge.Close()
	a := newClaudeWebAdapterForTest(bridge.URL + "/internal/claudeweb/v1")
	req, _ := http.NewRequest(http.MethodPost, "internal://claudeweb/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer client-supplied")
	if err := a.PrepareRequest(context.Background(), req, Account{ID: 9}, "sessionKey=sk-ant-1", SurfaceOpenAI); err != nil {
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
