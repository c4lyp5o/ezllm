package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/registration"
)

// The default key tester sends the key being tested; a provider redirecting
// to another origin must not hand it on.
func TestDefaultTesterRefusesCrossOriginRedirects(t *testing.T) {
	leaked := make(chan string, 16)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked <- r.Header.Get("X-Api-Key") + r.Header.Get("Authorization")
	}))
	defer collector.Close()
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, collector.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}, func(o *Options) { o.Tester = nil })
	acct := provider.Account{Name: "redir", Namespace: "redir", Kind: provider.KindAnthropicCompat, BaseURL: h.upstream.URL}
	h.server.tester.Run(context.Background(), acct, registration.Options{APIKey: "dummy-tested-key", SkipInference: true})
	select {
	case got := <-leaked:
		t.Fatalf("collector on another origin received credentials %q", got)
	default:
	}
}
