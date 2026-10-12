package registration

// Key-test probe behaviour babe asked for (2026-10-12):
//   - Gemini retires a model with a 404 while still cataloguing it — the probe
//     must advance to the next candidate instead of failing a good key.
//   - OpenAI answers 429 insufficient_quota to a VALID key with an empty
//     balance. That proves the credential; it must not reject the key.
//   - A 401 is still a hard credential verdict, and retired non-chat ids
//     (embeddings, image, TTS) are never probe candidates in the first place.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

func TestPickProbeModelsSkipsNonChatOrdersAndCaps(t *testing.T) {
	acct := provider.Account{Kind: provider.KindOpenAICompatible, BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai"}
	models := []provider.ModelInfo{
		{ID: "models/gemini-embedding-001"},
		{ID: "models/gemini-2.5-flash-image"},
		{ID: "models/gemini-2.5-flash"},
		{ID: "models/gemini-3.8-flash"},
		{ID: "models/gemini-3.8-pro"},
		{ID: "models/gemini-tts"},
	}
	got := pickProbeModels(models, acct)
	if len(got) != probeCandidates {
		t.Fatalf("candidates = %v, want %d entries", got, probeCandidates)
	}
	for _, id := range got {
		if !chatsLikeProbe(id) {
			t.Fatalf("non-chat model %q was offered as a probe candidate", id)
		}
	}
	if got[0] != "models/gemini-2.5-flash" {
		t.Fatalf("first candidate = %q (catalog order decides among 'flash' matches)", got[0])
	}
	// The single-candidate view stays the first candidate.
	if pickProbeModel(models, acct) != got[0] {
		t.Fatalf("pickProbeModel = %q, want %q", pickProbeModel(models, acct), got[0])
	}
}

func TestProbeAdvancesWhenTheFirstModelIsRetired(t *testing.T) {
	const key = "gemini-valid-key"
	client := &http.Client{Transport: retiredFirstTransport{key: key}}
	registry := provider.NewRegistry(client)
	adapter, err := registry.Get(provider.KindGeminiOpenAI)
	if err != nil {
		t.Fatal(err)
	}
	tester := NewTester(client, registry, nil)
	acct := provider.Account{Kind: provider.KindGeminiOpenAI, BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", Namespace: "google-ai-studio"}
	res := &TestResult{Steps: []Step{}}

	ok, failure := tester.inferenceStep(context.Background(), adapter, acct, key,
		[]string{"models/gemini-2.5-flash", "models/gemini-3.8-flash"}, &pacer{}, res)
	if !ok || failure != nil {
		t.Fatalf("a retired model must not fail the key test: ok=%v failure=%+v", ok, failure)
	}
	joined := stepDetails(res.Steps)
	if !strings.Contains(joined, "models/gemini-2.5-flash unavailable") ||
		!strings.Contains(joined, "falling back to models/gemini-3.8-flash") {
		t.Fatalf("fallback trail missing from steps: %s", joined)
	}
	if !strings.Contains(joined, "models/gemini-3.8-flash") {
		t.Fatalf("the serving model is not in the steps: %s", joined)
	}
}

func TestQuotaExhaustedProvesKeyWithoutSpending(t *testing.T) {
	const key = "openai-valid-key-no-credits"
	client := &http.Client{Transport: quotaExhaustedTransport{key: key}}
	registry := provider.NewRegistry(client)
	adapter, err := registry.Get(provider.KindOpenAICompatible)
	if err != nil {
		t.Fatal(err)
	}
	tester := NewTester(client, registry, nil)
	acct := provider.Account{Kind: provider.KindOpenAICompatible, BaseURL: "https://api.openai.com/v1", Namespace: "openai"}
	res := &TestResult{Steps: []Step{}}

	ok, failure := tester.inferenceStep(context.Background(), adapter, acct, key, []string{"gpt-5-nano"}, &pacer{}, res)
	if !ok || failure != nil {
		t.Fatalf("insufficient_quota on a valid key must pass: ok=%v failure=%+v", ok, failure)
	}
	joined := stepDetails(res.Steps)
	if !strings.Contains(joined, "credential accepted") || !strings.Contains(joined, "no remaining quota") {
		t.Fatalf("the quota verdict is not explained: %s", joined)
	}
}

func TestAuthRejectStillFailsAndRetiredCandidatesEndTheTest(t *testing.T) {
	const key = "gemini-valid-key"

	// A 401 keeps its meaning even though the new branches exist.
	authClient := &http.Client{Transport: fixedStatusTransport{key: key, status: http.StatusUnauthorized,
		body: `{"error":{"message":"API key not valid. Please pass a valid API key."}}`}}
	reg := provider.NewRegistry(authClient)
	adapter, err := reg.Get(provider.KindGeminiOpenAI)
	if err != nil {
		t.Fatal(err)
	}
	tester := NewTester(authClient, reg, nil)
	acct := provider.Account{Kind: provider.KindGeminiOpenAI, BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", Namespace: "google-ai-studio"}
	res := &TestResult{Steps: []Step{}}
	if ok, failure := tester.inferenceStep(context.Background(), adapter, acct, key,
		[]string{"models/gemini-2.5-flash", "models/gemini-3.8-flash"}, &pacer{}, res); ok || failure == nil {
		t.Fatalf("a 401 must still fail the key test: ok=%v failure=%+v", ok, failure)
	}

	// Every candidate retired → the test fails, with the LAST model's reason.
	retiredClient := &http.Client{Transport: retiredAllTransport{key: key}}
	reg2 := provider.NewRegistry(retiredClient)
	adapter2, err := reg2.Get(provider.KindGeminiOpenAI)
	if err != nil {
		t.Fatal(err)
	}
	tester2 := NewTester(retiredClient, reg2, nil)
	res2 := &TestResult{Steps: []Step{}}
	ok, failure := tester2.inferenceStep(context.Background(), adapter2, acct, key,
		[]string{"models/gemini-2.5-flash", "models/gemini-3.8-flash"}, &pacer{}, res2)
	if ok || failure == nil {
		t.Fatalf("all-retired candidates must fail: ok=%v failure=%+v", ok, failure)
	}
	if !strings.Contains(failure.Message, "models/gemini-3.8-flash") {
		t.Fatalf("failure should name the last candidate: %+v", failure)
	}
}

func stepDetails(steps []Step) string {
	var b strings.Builder
	for _, s := range steps {
		b.WriteString(s.Detail)
		b.WriteString(" | ")
	}
	return b.String()
}

func probePathOf(req *http.Request) string {
	u, err := url.Parse(req.URL.String())
	if err != nil {
		return req.URL.Path
	}
	return u.Path
}

// retiredFirstTransport: the first catalog candidate 404s as retired (exactly
// Gemini's wording), the next answers a normal completion.
type retiredFirstTransport struct{ key string }

func (tr retiredFirstTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !strings.HasPrefix(probePathOf(req), "/v1beta/openai/") {
		return nil, fmt.Errorf("unexpected probe path: %s", req.URL)
	}
	if req.Header.Get("Authorization") != "Bearer "+tr.key {
		return jsonHTTPResponse(req, http.StatusUnauthorized, `{"error":{"message":"API key not valid."}}`), nil
	}
	body, _ := io.ReadAll(req.Body)
	var payload struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &payload)
	switch payload.Model {
	case "models/gemini-2.5-flash":
		return jsonHTTPResponse(req, http.StatusNotFound,
			`{"error":{"code":404,"message":"This model models/gemini-2.5-flash is no longer available to new users. Please update your code to use models/gemini-3.8-flash for the latest features and improvements.","status":"NOT_FOUND"}}`), nil
	case "models/gemini-3.8-flash":
		return jsonHTTPResponse(req, http.StatusOK,
			`{"id":"chatcmpl-1","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`), nil
	default:
		return jsonHTTPResponse(req, http.StatusBadRequest, `{"error":{"message":"unexpected model in test"}}`), nil
	}
}

// retiredAllTransport: every candidate is retired.
type retiredAllTransport struct{ key string }

func (tr retiredAllTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Authorization") != "Bearer "+tr.key {
		return jsonHTTPResponse(req, http.StatusUnauthorized, `{"error":{"message":"API key not valid."}}`), nil
	}
	body, _ := io.ReadAll(req.Body)
	var payload struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &payload)
	return jsonHTTPResponse(req, http.StatusNotFound,
		fmt.Sprintf(`{"error":{"code":404,"message":"This model %s is no longer available to new users.","status":"NOT_FOUND"}}`, payload.Model)), nil
}

// quotaExhaustedTransport: a valid key whose org has no credits — OpenAI's
// own 429 body.
type quotaExhaustedTransport struct{ key string }

func (tr quotaExhaustedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Authorization") != "Bearer "+tr.key {
		return jsonHTTPResponse(req, http.StatusUnauthorized, `{"error":{"message":"Incorrect API key provided."}}`), nil
	}
	return jsonHTTPResponse(req, http.StatusTooManyRequests,
		`{"error":{"message":"You exceeded your current quota, please check your plan and billing details.","type":"insufficient_quota","param":null,"code":"insufficient_quota"}}`), nil
}

// fixedStatusTransport answers one fixed status for any authenticated call.
type fixedStatusTransport struct {
	key    string
	status int
	body   string
}

func (tr fixedStatusTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Authorization") != "Bearer "+tr.key {
		return jsonHTTPResponse(req, http.StatusUnauthorized, `{"error":{"message":"API key not valid."}}`), nil
	}
	return jsonHTTPResponse(req, tr.status, tr.body), nil
}
