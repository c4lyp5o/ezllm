package registration

// The Google AI Studio case (docx item 9): probing fails with 503 "high
// demand" on the GENERATION probe while the credential is perfectly fine.
// That is a capacity signal, not a credential verdict — and before this
// rule, the whole key test failed with the misleading hint "the provider
// rejected a minimal test request". When some OTHER authenticated endpoint
// has already proven the key (here: the catalog, which rejects a
// deliberately-wrong key), the test must pass with an honest note and a
// verifiable action to verify generation later.
//
// The counter-test (TestCapacityFailureWithPublicCatalogStillFails) pins
// the other edge: a catalog that answers 200 to a garbage key proves
// nothing, so the capacity failure stays fatal — a key that is never proven
// is a key we must not store.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// fakeGemini mirrors Google AI Studio's OpenAI-compatible surface:
//   - catalog authenticates: real key lists models, garbage gets 400
//     (KindGeminiOpenAI declares 400 alongside 401/403 for bad keys)
//   - no /usage oracle → auth defers to inference
//   - generation answers with whatever genStatus the test injected
//
// publicCatalog flips the catalog to answer 200 for garbage as well —
// the N1-class "public catalog proves nothing" case.
func fakeGemini(t *testing.T, genStatus int, publicCatalog bool) (*httptest.Server, string) {
	t.Helper()
	const validKey = "AIza-valid-key-for-tests"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		valid := bearer == validKey
		if strings.HasSuffix(r.URL.Path, "/models") {
			if !valid && !publicCatalog {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"error":{"message":"API key not valid. Please pass a valid API key.","status":"INVALID_ARGUMENT"}}`))
				return
			}
			w.Write([]byte(`{"object":"list","data":[{"id":"gemini-2.5-flash","object":"model"},{"id":"gemini-2.5-pro","object":"model"}]}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/usage") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/chat/completions") {
			w.WriteHeader(genStatus)
			w.Write([]byte(`{"error":{"code":503,"status":"UNAVAILABLE","message":"The model is overloaded. Please try again later."}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv, validKey
}

func geminiAccount(baseURL string) provider.Account {
	return provider.Account{
		ID:      9,
		Name:    "google-ai-studio",
		Kind:    provider.KindGeminiOpenAI,
		BaseURL: baseURL + "/v1/openai",
		Enabled: true,
	}
}

func stepDetail(res *TestResult, step string) (string, bool) {
	last := ""
	found := false
	for _, s := range res.Steps {
		if s.Step == step {
			last = s.Detail
			found = true
		}
	}
	return last, found
}

func TestCapacityFailurePassesWhenCatalogProvesKey(t *testing.T) {
	srv, key := fakeGemini(t, http.StatusServiceUnavailable, false)
	client := srv.Client()
	tester := NewTester(client, provider.NewRegistry(client), nil)

	res := tester.Run(context.Background(), geminiAccount(srv.URL), Options{
		Label:          "gemini",
		APIKey:         key,
		ProtocolInline: false,
	})

	if res.Fail != nil {
		t.Fatalf("503 capacity failure sank a proven key: step=%s msg=%q", res.Fail.Step, res.Fail.Message)
	}
	if !res.OK {
		t.Fatalf("expected OK=true with capacity note, got steps=%+v", res.Steps)
	}

	// The UI's by-step map keeps the LAST entry per step, so the final
	// inference entry must be the OK note, not the 503 failure.
	detail, ok := stepDetail(res, StepInference)
	if !ok {
		t.Fatalf("no inference step recorded: steps=%+v", res.Steps)
	}
	if !strings.Contains(detail, "generation unverified") {
		t.Fatalf("final inference detail = %q, want the honest unverified note", detail)
	}
	if !strings.Contains(detail, "HTTP 503") {
		t.Fatalf("note must name the capacity status it saw: %q", detail)
	}
	// The note must NOT restate the failed step's rejection-flavored text —
	// "generation probe failed" + the provider's message live in the
	// OK:false entry, not in the verdict line.
	if strings.Contains(detail, "generation probe failed") || strings.Contains(detail, "overloaded") {
		t.Fatalf("note leaks the raw failure text: %q", detail)
	}

	// The failed (OK:false) step must REMAIN in the trail — hiding it would
	// falsify the audit. Verify both entries exist.
	sawFailure, sawNote := false, false
	for _, s := range res.Steps {
		if s.Step == StepInference && !s.OK && strings.Contains(s.Detail, "generation probe failed") {
			sawFailure = true
		}
		if s.Step == StepInference && s.OK && strings.Contains(s.Detail, "generation unverified") {
			sawNote = true
		}
	}
	if !sawFailure || !sawNote {
		t.Fatalf("audit trail incomplete (failure=%v note=%v): steps=%+v", sawFailure, sawNote, res.Steps)
	}

	// The auth step must record that the CALIBRATION proved the key.
	auth, ok := stepDetail(res, StepAuth)
	if !ok || !strings.Contains(auth, "catalog rejected a fake key") {
		t.Fatalf("auth step = %q (ok=%v), want catalog-calibration proof", auth, ok)
	}
}

func TestCapacityFailureWithPublicCatalogStillFails(t *testing.T) {
	srv, key := fakeGemini(t, http.StatusServiceUnavailable, true)
	client := srv.Client()
	tester := NewTester(client, provider.NewRegistry(client), nil)

	res := tester.Run(context.Background(), geminiAccount(srv.URL), Options{
		Label:          "gemini",
		APIKey:         key,
		ProtocolInline: false,
	})

	if res.Fail == nil {
		t.Fatalf("503 with a public catalog must NOT be stored as a proven key; detail=%q", res.Detail)
	}
	if res.OK {
		t.Fatalf("OK must stay false when nothing proves the key")
	}
	if !strings.Contains(res.Detail, "failed at inference") {
		t.Fatalf("detail = %q, want failed-at-inference", res.Detail)
	}
	// The hint must not claim a credential rejection for capacity.
	if strings.Contains(res.Fail.Hint, "rejected a minimal test request") {
		t.Fatalf("capacity failure still carries rejection hint: %q", res.Fail.Hint)
	}
}

// A 403 on generation is an auth verdict no matter what the catalog says:
// the calibration proof may not launder it into a pass.
func TestAuthRejectOnGenerationStillFailsDespiteProof(t *testing.T) {
	const validKey = "AIza-valid-key-for-tests"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		valid := bearer == validKey
		if strings.HasSuffix(r.URL.Path, "/models") {
			if !valid {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"error":{"status":"INVALID_ARGUMENT"}}`))
				return
			}
			w.Write([]byte(`{"object":"list","data":[{"id":"gemini-2.5-flash"}]}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/chat/completions") {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":{"message":"Requests to this API are forbidden"}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	client := srv.Client()
	tester := NewTester(client, provider.NewRegistry(client), nil)

	res := tester.Run(context.Background(), geminiAccount(srv.URL), Options{
		Label:          "gemini",
		APIKey:         validKey,
		ProtocolInline: false,
	})

	if res.Fail == nil {
		t.Fatalf("403 on generation must fail even with a calibrating catalog; detail=%q", res.Detail)
	}
	if res.OK {
		t.Fatalf("OK must stay false")
	}
}

// A non-capacity 400 with a public catalog: not retryable, no proof — fail,
// and the hint must still describe the request verdict honestly.
func TestRetryableFailureWithoutProofUsesCapacityHint(t *testing.T) {
	srv, key := fakeGemini(t, http.StatusBadRequest, true)
	client := srv.Client()
	tester := NewTester(client, provider.NewRegistry(client), nil)

	res := tester.Run(context.Background(), geminiAccount(srv.URL), Options{
		Label:          "gemini",
		APIKey:         key,
		ProtocolInline: false,
	})

	if res.Fail == nil {
		t.Fatalf("400 on generation with no proof must fail")
	}
	if strings.Contains(res.Fail.Hint, "rate-limited or unavailable") {
		t.Fatalf("400 must not use the capacity hint: %q", res.Fail.Hint)
	}
}
