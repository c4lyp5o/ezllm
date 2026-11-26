package registration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// A probe sends the headers live traffic sends: an account's custom header
// cannot replace the bridge secret a web adapter stamps.
func TestProbeCustomHeadersCannotReplaceBridgeHeaders(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get(provider.BridgeSecretHeader)
		w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()
	adapter := provider.NewClaudeWeb(srv.URL+"/internal/claudeweb/v1", srv.Client())
	acct := provider.Account{ID: 7, Kind: provider.KindClaudeWeb, CustomHeaders: map[string]string{provider.BridgeSecretHeader: "operator-value"}}
	if _, _, err := doProbe(context.Background(), srv.Client(), adapter, provider.AuthProbe{Method: http.MethodGet, Path: "/models"}, acct, "k"); err != nil {
		t.Fatal(err)
	}
	if got != provider.BridgeSecret() {
		t.Fatalf("bridge secret on the probe = %q, want the process secret", got)
	}
}
