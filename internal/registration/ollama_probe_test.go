package registration

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

func TestOllamaCloudProbePrefersDocumentedFreeModels(t *testing.T) {
	acct := provider.Account{
		Kind:    provider.KindOpenAICompatible,
		BaseURL: "https://ollama.com/v1",
	}
	models := []provider.ModelInfo{
		{ID: "deepseek-v4.1-flash"}, // listed, but paid on this user's plan
		{ID: "minimax-m3"},          // also listed, but paid
		{ID: "gemma4:31b"},
		{ID: "gpt-oss:20b"},
		{ID: "nemotron-3-nano:30b"},
	}
	if got := pickProbeModel(models, acct); got != "gemma4:31b" {
		t.Fatalf("pickProbeModel() = %q, want first documented free model gemma4:31b", got)
	}
}

func TestOllamaCloudProbeUsesNextFreeModelWhenFirstAbsent(t *testing.T) {
	acct := provider.Account{Kind: provider.KindOpenAICompatible, BaseURL: "https://ollama.com/v1"}
	models := []provider.ModelInfo{{ID: "deepseek-v4.1-flash"}, {ID: "gpt-oss:20b"}, {ID: "gemma4:31b"}}
	if got := pickProbeModel(models, acct); got != "gemma4:31b" {
		t.Fatalf("pickProbeModel() = %q, want gemma4:31b", got)
	}
	models = []provider.ModelInfo{{ID: "deepseek-v4.1-flash"}, {ID: "gpt-oss:20b"}}
	if got := pickProbeModel(models, acct); got != "gpt-oss:20b" {
		t.Fatalf("fallback pickProbeModel() = %q, want gpt-oss:20b", got)
	}
}

func TestOllamaCloudDetectionIsHostAndKindScoped(t *testing.T) {
	for _, tc := range []struct {
		name string
		acct provider.Account
		want bool
	}{
		{"canonical host", provider.Account{Kind: provider.KindOpenAICompatible, BaseURL: "https://ollama.com/v1"}, true},
		{"case-insensitive host", provider.Account{Kind: provider.KindOpenAICompatible, BaseURL: "https://OLLAMA.COM/v1"}, true},
		{"subdomain is not canonical", provider.Account{Kind: provider.KindOpenAICompatible, BaseURL: "https://ollama.com.attacker.example/v1"}, false},
		{"wrong scheme rejected", provider.Account{Kind: provider.KindOpenAICompatible, BaseURL: "http://ollama.com/v1"}, false},
		{"different kind", provider.Account{Kind: provider.KindAnthropicCompat, BaseURL: "https://ollama.com"}, false},
		{"other OpenAI endpoint", provider.Account{Kind: provider.KindOpenAICompatible, BaseURL: "https://api.openai.com/v1"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isOllamaCloud(tc.acct); got != tc.want {
				t.Fatalf("isOllamaCloud() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestOllamaPlanGatedResponseProvesCredentialButAuthRejectDoesNot(t *testing.T) {
	gated := []byte(`{"error":"this model is not included in your free usage, add usage credits to pay as you go"}`)
	if !isPlanGatedError(http.StatusPaymentRequired, gated) {
		t.Fatal("Ollama plan-gated 402 should be recognized")
	}
	if isPlanGatedError(http.StatusOK, gated) {
		t.Fatal("a 200 error payload is not a plan-gated HTTP failure")
	}
	if isPlanGatedError(http.StatusForbidden, gated) {
		t.Fatal("401/403 must remain hard auth failures even if body text mentions plan")
	}
	if isPlanGatedError(http.StatusPaymentRequired, []byte(`{"error":"invalid api key"}`)) {
		t.Fatal("unrelated payment error must not be treated as proof")
	}
}

func TestOllamaPlanGatedGenerationIsAcceptedWithClearStep(t *testing.T) {
	const key = "ollama-valid-key"
	client := &http.Client{Transport: ollamaPlanGateTransport{key: key}}
	registry := provider.NewRegistry(client)
	adapter, err := registry.Get(provider.KindOpenAICompatible)
	if err != nil {
		t.Fatal(err)
	}
	tester := NewTester(client, registry, nil)
	acct := provider.Account{Kind: provider.KindOpenAICompatible, BaseURL: "https://ollama.com/v1", Namespace: "test-ollama"}
	res := &TestResult{}
	ok, failure := tester.inferenceStep(context.Background(), adapter, acct, key, []string{"deepseek-v4.1-flash"}, &pacer{}, res)
	if !ok || failure != nil {
		t.Fatalf("plan-gated valid key should be accepted: ok=%v failure=%+v", ok, failure)
	}
	if len(res.Steps) != 1 || !strings.Contains(res.Steps[0].Detail, "credential accepted") {
		t.Fatalf("missing explanatory success step: %+v", res.Steps)
	}
}

type ollamaPlanGateTransport struct{ key string }

func (tr ollamaPlanGateTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Hostname() != "ollama.com" || req.URL.Path != "/v1/chat/completions" {
		return nil, fmt.Errorf("unexpected probe URL: %s", req.URL)
	}
	if req.Header.Get("Authorization") != "Bearer "+tr.key {
		return jsonHTTPResponse(req, http.StatusUnauthorized, `{"error":"invalid api key"}`), nil
	}
	return jsonHTTPResponse(req, http.StatusPaymentRequired,
		`{"error":"this model is not included in your free usage; add usage credits"}`), nil
}

func jsonHTTPResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

func TestOllamaProbeModelDoesNotSelectPaidModelWhenNoFreeModelListed(t *testing.T) {
	acct := provider.Account{Kind: provider.KindOpenAICompatible, BaseURL: "https://ollama.com/v1"}
	models := []provider.ModelInfo{{ID: "deepseek-v4.1-flash"}, {ID: "minimax-m3"}}
	// A catalog that lists only paid models should not cause the generic picker
	// to select one and reject the valid free-plan key. Returning no model makes
	// the existing public-catalog auth calibration fail closed rather than store
	// an unproven credential.
	if got := pickProbeModel(models, acct); got != "" {
		t.Fatalf("no free candidate should return empty, got paid model %q", got)
	}
	if !isOllamaCloud(acct) {
		t.Fatal("expected canonical Ollama account")
	}
}
