package provider

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// Verbatim live body, GET api.openai.com/v1/dashboard/billing/credit_grants
// (2026-10-20) — an overspent grant, so total_available is NEGATIVE.
const openAICreditGrantsLiveBody = `{
  "object": "credit_summary",
  "total_granted": 5.0,
  "total_used": 6.39474612,
  "total_available": -1.39474612,
  "total_paid_available": 0.0,
  "grants": {
    "object": "list",
    "data": [
      {
        "object": "credit_summary",
        "id": "in_1U1KfkC6h1nxGoI3mEnzc7Tp",
        "grant_amount": 5.0,
        "used_amount": 5.0,
        "effective_at": 1785974400.0,
        "expires_at": null,
        "is_scheduled": false,
        "is_held_for_prepaid_transition": false
      }
    ]
  }
}`

func TestParseOpenAICreditGrants(t *testing.T) {
	q := parseOpenAICreditGrants(openAICreditGrantsLiveBody)
	if q.Kind != QuotaMoney || q.BalanceUnit != "USD" {
		t.Fatalf("kind=%q unit=%q", q.Kind, q.BalanceUnit)
	}
	if q.Balance == nil || *q.Balance > -1.39 || *q.Balance < -1.40 {
		t.Errorf("balance = %v, want ~-1.3947 (total_available, not total_granted)", q.Balance)
	}
	if q.CostTotal == nil || *q.CostTotal < 6.39 || *q.CostTotal > 6.40 {
		t.Errorf("cost_total = %v, want ~6.3947", q.CostTotal)
	}
}

func TestParseOpenAICreditGrantsGarbage(t *testing.T) {
	for _, body := range []string{``, `<html>`, `{}`, `{"error":{"message":"bad"}}`, `{"object":"credit_summary"}`, `{"total_available":1}`} {
		if q := parseOpenAICreditGrants(body); q.Kind != "" {
			t.Errorf("%q → kind %q, want zero Quota", body, q.Kind)
		}
	}
}

func TestOpenAISessionToken(t *testing.T) {
	for in, want := range map[string]string{"": "", "sess-abc": "sess-abc", " Bearer sess-abc ": "sess-abc", "bearer sess-abc": "sess-abc"} {
		if got := openAISessionToken(in); got != want {
			t.Errorf("openAISessionToken(%q) = %q, want %q", in, got, want)
		}
	}
}

// ReadQuota dials the origin-rooted dashboard path with the SESSION token and
// never the sk- inference key; with no session token it makes no call.
func TestReadQuotaOpenAI(t *testing.T) {
	acct := Account{Kind: KindOpenAICompatible, BaseURL: "https://api.openai.com/v1"}
	var calls int
	var gotPath, gotAuth string
	a := &openAICompatible{client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(openAICreditGrantsLiveBody)),
		}, nil
	})}}

	if _, err := a.ReadQuota(t.Context(), acct, "sk-inference"); err != ErrQuotaUnsupported || calls != 0 {
		t.Fatalf("no session token: err=%v calls=%d, want ErrQuotaUnsupported and no call", err, calls)
	}
	acct.ManagementKey = "sess-live"
	q, err := a.ReadQuota(t.Context(), acct, "sk-inference")
	if err != nil || q.Kind != QuotaMoney {
		t.Fatalf("ReadQuota: kind=%q err=%v", q.Kind, err)
	}
	if gotPath != "/v1/dashboard/billing/credit_grants" {
		t.Errorf("dialed %q", gotPath)
	}
	if gotAuth != "Bearer sess-live" {
		t.Errorf("auth = %q, want the session token (never the sk- key)", gotAuth)
	}
}
