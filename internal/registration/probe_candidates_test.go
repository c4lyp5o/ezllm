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

// ── NVIDIA NIM (measured live 2026-10-13, free-credits account) ──────────────
// The generic buckets pick deepseek-ai/deepseek-v4.1-flash first on this host;
// it never answered a 16-token probe (90s+), glm-5.3-flash took 43.7s — both
// past defaultStepTimeout — while the preferred ids answer in 0.6s/1.7s/6.2s.

func TestNIMCatalogPicksMeasuredFastCandidates(t *testing.T) {
	acct := provider.Account{Kind: provider.KindOpenAICompatible, BaseURL: "https://integrate.api.nvidia.com/v1"}
	models := []provider.ModelInfo{
		{ID: "deepseek-ai/deepseek-v4.1-flash"},       // generic bucket's first pick — >90s live
		{ID: "z-ai/glm-5.3-flash"},                    // "flash" bucket — 43.7s live
		{ID: "nvidia/nemotron-3.5-lightning-30b-a3b"}, // 0.6s live
		{ID: "meta/llama-3.2-11b-vision-instruct"},    // 1.7s live
		{ID: "openai/gpt-oss-20b"},                    // 6.2s live
		{ID: "nvidia/embed-qa-4"},                     // non-chat, must never appear
	}
	want := []string{
		"nvidia/nemotron-3.5-lightning-30b-a3b",
		"meta/llama-3.2-11b-vision-instruct",
		"openai/gpt-oss-20b",
	}
	got := pickProbeModels(models, acct)
	if len(got) != len(want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidate[%d] = %q, want %q (full list %v)", i, got[i], want[i], got)
		}
	}
	if pickProbeModel(models, acct) != want[0] {
		t.Fatalf("single-candidate view = %q, want %q", pickProbeModel(models, acct), want[0])
	}
}

func TestNIMWithoutPreferredModelsFallsBackToGeneric(t *testing.T) {
	acct := provider.Account{Kind: provider.KindOpenAICompatible, BaseURL: "https://integrate.api.nvidia.com/v1"}
	models := []provider.ModelInfo{
		{ID: "deepseek-ai/deepseek-v4.1-flash"},
		{ID: "nvidia/llama-3.1-nemotron-70b-instruct"},
	}
	got := pickProbeModels(models, acct)
	if len(got) == 0 || got[0] != "deepseek-ai/deepseek-v4.1-flash" {
		t.Fatalf("fallback candidates = %v, want the generic bucket's pick first", got)
	}
}

func TestNIMDetectionIsHostScoped(t *testing.T) {
	yes := provider.Account{Kind: provider.KindOpenAICompatible, BaseURL: "https://integrate.api.nvidia.com/v1"}
	if !isNVIDIANIM(yes) {
		t.Fatal("the NIM preset base URL must be detected")
	}
	no := []provider.Account{
		{Kind: provider.KindOpenAICompatible, BaseURL: "https://api.openai.com/v1"},
		{Kind: provider.KindOpenAICompatible, BaseURL: "http://integrate.api.nvidia.com/v1"}, // not https
		{Kind: provider.KindAnthropicCompat, BaseURL: "https://integrate.api.nvidia.com/v1"},
	}
	for _, acct := range no {
		if isNVIDIANIM(acct) {
			t.Fatalf("must not match kind=%s base=%q", acct.Kind, acct.BaseURL)
		}
	}
}

// hangFirstTransport: candidate #1 never answers — the live NVIDIA NIM shape
// (deepseek-v4.1-flash outlives the probe deadline) — candidate #2 answers.
type hangFirstTransport struct{ key string }

func (tr hangFirstTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Authorization") != "Bearer "+tr.key {
		return jsonHTTPResponse(req, http.StatusUnauthorized, `{"error":{"message":"bad key"}}`), nil
	}
	body, _ := io.ReadAll(req.Body)
	var payload struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &payload)
	if payload.Model == "slow-model" {
		// What net/http returns when the caller's context deadline fires.
		return nil, fmt.Errorf(`Post "https://integrate.api.nvidia.com/v1/chat/completions": context deadline exceeded`)
	}
	return jsonHTTPResponse(req, http.StatusOK,
		`{"id":"chatcmpl-1","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`), nil
}

func TestTimeoutOnOneModelAdvancesInsteadOfFailingTheKey(t *testing.T) {
	const key = "nim-valid-key"
	client := &http.Client{Transport: hangFirstTransport{key: key}}
	registry := provider.NewRegistry(client)
	adapter, err := registry.Get(provider.KindOpenAICompatible)
	if err != nil {
		t.Fatal(err)
	}
	tester := NewTester(client, registry, nil)
	acct := provider.Account{Kind: provider.KindOpenAICompatible, BaseURL: "https://integrate.api.nvidia.com/v1", Namespace: "nvidia-nim"}
	res := &TestResult{Steps: []Step{}}

	ok, failure := tester.inferenceStep(context.Background(), adapter, acct, key,
		[]string{"slow-model", "fast-model"}, &pacer{}, res)
	if !ok || failure != nil {
		t.Fatalf("a timed-out model must not fail a valid key: ok=%v failure=%+v", ok, failure)
	}
	joined := stepDetails(res.Steps)
	if !strings.Contains(joined, "slow-model did not answer within") ||
		!strings.Contains(joined, "falling back to fast-model") {
		t.Fatalf("timeout fallback trail missing from steps: %s", joined)
	}
}

// hangAllTransport: every candidate times out — the honest verdict is "no
// answer", never "the provider rejected".
type hangAllTransport struct{ key string }

func (tr hangAllTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf(`Post "https://integrate.api.nvidia.com/v1/chat/completions": context deadline exceeded`)
}

func TestAllCandidatesTimingOutFailsHonestNotAsRejection(t *testing.T) {
	const key = "nim-valid-key"
	client := &http.Client{Transport: hangAllTransport{key: key}}
	registry := provider.NewRegistry(client)
	adapter, err := registry.Get(provider.KindOpenAICompatible)
	if err != nil {
		t.Fatal(err)
	}
	tester := NewTester(client, registry, nil)
	acct := provider.Account{Kind: provider.KindOpenAICompatible, BaseURL: "https://integrate.api.nvidia.com/v1", Namespace: "nvidia-nim"}
	res := &TestResult{Steps: []Step{}}

	ok, failure := tester.inferenceStep(context.Background(), adapter, acct, key,
		[]string{"slow-a", "slow-b"}, &pacer{}, res)
	if ok || failure == nil {
		t.Fatalf("all-timeout must fail the test: ok=%v failure=%+v", ok, failure)
	}
	if !strings.Contains(failure.Message, "slow-b did not answer within") {
		t.Fatalf("failure should name the last candidate and the honest cause: %+v", failure)
	}
	if strings.Contains(failure.Hint, "The provider rejected") {
		t.Fatalf("a timeout is not a provider rejection: %+v", failure)
	}
}
