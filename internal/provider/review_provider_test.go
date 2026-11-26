package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

// OpenAI's reasoning models refuse the probe's max_tokens by name. That is a
// refused option, not a missing wire.
func TestProbeParameterRefusalIsNotAWireGap(t *testing.T) {
	body := `{"error":{"message":"Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead.","type":"invalid_request_error","param":"max_tokens","code":"unsupported_parameter"}}`
	if v, _ := ClassifyProbeResult(400, body); v != VerdictUnknown {
		t.Fatalf("verdict = %v, want unknown for a refused parameter", v)
	}
	// A real protocol gap still reads as unsupported.
	if v, _ := ClassifyProbeResult(400, `{"error":{"message":"model is not supported for format openai"}}`); v != VerdictUnsupported {
		t.Fatalf("protocol gap verdict = %v, want unsupported", v)
	}
}

// Gemini's per-minute limit mentions "quota" but clears in a minute.
func TestErrorCauseGeminiPerMinuteIsRateLimited(t *testing.T) {
	body := []byte(`{"error":{"code":429,"message":"Quota exceeded for quota metric 'Generate Content API requests per minute' and limit 'GenerateContent request limit per minute for a region' of service 'generativelanguage.googleapis.com'","status":"RESOURCE_EXHAUSTED"}}`)
	if got := ErrorCause(429, body); got != "rate_limited" {
		t.Fatalf("cause = %q, want rate_limited", got)
	}
	if got := ErrorCause(429, []byte(`{"error":{"code":"insufficient_quota","message":"You exceeded your current quota"}}`)); got != "quota_exhausted" {
		t.Fatalf("spent balance cause = %q, want quota_exhausted", got)
	}
}

// A model that takes no reasoning refuses the field; that must not be learned
// as "reasoning required".
func TestParseReasoningRuleFieldNotAllowedIsNotRequired(t *testing.T) {
	for _, body := range []string{
		`{"error":{"message":"Extra inputs are not permitted: reasoning_effort is not allowed for this model"}}`,
		`{"error":{"message":"thinking is not allowed on non-reasoning models"}}`,
	} {
		if r := ParseReasoningRule(400, []byte(body)); r.Required {
			t.Fatalf("%s: learned Required, want nothing", body)
		}
	}
	if r := ParseReasoningRule(400, []byte(bodyMiniMaxAdaptive)); !r.Required {
		t.Fatal("'reasoning effort none is not allowed' must still read as required")
	}
	if r := ParseReasoningRule(400, []byte(`{"error":{"message":"reasoning_effort 'off' is not allowed for this model"}}`)); !r.Required {
		t.Fatal("'off' is not allowed must read as required")
	}
}

// A query string goes to the bridge as a query, not as an escaped path.
func TestWebAdaptersKeepTheQuery(t *testing.T) {
	for _, a := range []Adapter{
		newClaudeWebAdapterForTest("http://127.0.0.1:1/internal/claudeweb/v1"),
		NewGeminiWeb("http://127.0.0.1:1/internal/geminiweb/v1", http.DefaultClient),
	} {
		req := httptest.NewRequest(http.MethodPost, "http://sentinel/chat/completions?beta=true", nil)
		if err := a.PrepareRequest(context.Background(), req, Account{ID: 1}, "k", SurfaceChat); err != nil {
			t.Fatal(err)
		}
		if req.URL.RawQuery != "beta=true" || strings.Contains(req.URL.String(), "%3F") {
			t.Fatalf("%s: url = %s, want path /chat/completions and query beta=true", a.Kind(), req.URL)
		}
	}
}

// A nonsense exponent yields no balance instead of +Inf, which JSON rejects.
func TestPrepaidCreditsRejectsInfiniteBalance(t *testing.T) {
	q := parseClaudePrepaidCredits(`{"currency":"USD","balance":{"money":{"amount_minor":5,"exponent":-400}}}`)
	if q.Balance != nil {
		t.Fatalf("balance = %v, want none", *q.Balance)
	}
}

func TestTagQuotaRawKeepsUTF8(t *testing.T) {
	q := tagQuotaRaw(Quota{Raw: `{}`}, "note", strings.Repeat("a", 199)+"é"+"tail")
	if !utf8.ValidString(q.Raw) || strings.Contains(q.Raw, `�`) {
		t.Fatalf("raw = %q, split a rune", q.Raw)
	}
}
