package provider

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// Body shape captured live from GET platform.claude.com/api/organizations/{org}/prepaid/credits
// (2026-10-20): a $5.00 grant with $0.07 left. The tranche id is synthetic.
const claudePrepaidLiveBody = `{"amount":7,"currency":"USD","balance":{"money":null,"credits":{"amount_minor":7,"exponent":2}},"balance_credits":0,"amount_without_scoped_credits":7,"auto_reload_settings":null,"auto_reload_disabled_reason":null,"auto_reload_stripe_error_code":null,"pending_invoice_amount_cents":null,"last_paid_purchase_cents":500,"expiry_policy_months":null,"credit_expiry_policy":null,"auto_reload_terms_banner":null,"tranches":[{"remaining_amount_minor_units":6,"currency":"USD","expires_at":"2027-10-09T00:00:00Z","granted_amount_minor_units":500,"granted_at":"2026-10-08T14:04:12Z","remaining":{"money":null,"credits":{"amount_minor":6,"exponent":2}},"granted":{"money":null,"credits":{"amount_minor":500,"exponent":2}},"program_id":"api_credits","name":null,"id":"00000000-0000-4000-8000-000000000001","scope":{"kind":"unscoped","applies_to":[]}}],"promo_tranches":[],"next_expires_at":"2027-10-09T00:00:00Z"}`

const claudeTestOrg = "11111111-2222-4333-8444-555555555555"

func TestParseClaudePrepaidCredits(t *testing.T) {
	q := parseClaudePrepaidCredits(claudePrepaidLiveBody)
	if q.Kind != QuotaMoney || q.BalanceUnit != "USD" {
		t.Fatalf("kind=%q unit=%q", q.Kind, q.BalanceUnit)
	}
	if q.Balance == nil || *q.Balance < 0.0699 || *q.Balance > 0.0701 {
		t.Errorf("balance = %v, want 0.07 dollars (minor units / 10^exponent)", q.Balance)
	}
	if q.CostTotal == nil || *q.CostTotal < 4.929 || *q.CostTotal > 4.931 {
		t.Errorf("cost_total = %v, want ~4.93", q.CostTotal)
	}
}

func TestParseClaudePrepaidCreditsShapes(t *testing.T) {
	// money-shaped balance, no tranches → balance only
	q := parseClaudePrepaidCredits(`{"currency":"USD","balance":{"money":{"amount_minor":1250,"exponent":2},"credits":null}}`)
	if q.Balance == nil || *q.Balance != 12.5 || q.CostTotal != nil {
		t.Errorf("money shape: balance=%v cost=%v", q.Balance, q.CostTotal)
	}
	for _, body := range []string{``, `<html>`, `{}`, `{"error":{"type":"permission_error"}}`, `{"balance":{"money":null,"credits":null}}`} {
		if q := parseClaudePrepaidCredits(body); q.Kind != "" {
			t.Errorf("%q → kind %q, want zero Quota", body, q.Kind)
		}
	}
}

func TestClaudePlatformCreds(t *testing.T) {
	full := "a=1; sessionKey=sk-ant-sid02-x; lastActiveOrg=" + claudeTestOrg + "; b=2"
	c, org, ok := claudePlatformCreds(full)
	if !ok || c != "sessionKey=sk-ant-sid02-x" || org != claudeTestOrg {
		t.Fatalf("got %q %q %v", c, org, ok)
	}
	for _, in := range []string{
		"", "sk-ant-sid02-x", "sessionKey=sk-ant-sid02-x", "lastActiveOrg=" + claudeTestOrg,
		"sessionKey=x; lastActiveOrg=../../admin", "sessionKey=x; lastActiveOrg=" + claudeTestOrg + "/evil",
	} {
		if _, _, ok := claudePlatformCreds(in); ok {
			t.Errorf("%q: want refused (the org id is interpolated into a URL path)", in)
		}
	}
}

// ReadQuota: no session → no call; session → console host, org in the path,
// Cookie header, and the sk-ant inference key never sent.
func TestReadQuotaAnthropicPlatform(t *testing.T) {
	acct := Account{Kind: KindAnthropicCompat, BaseURL: "https://api.anthropic.com"}
	var calls int
	var gotHost, gotPath, gotCookie, gotAuth, gotXKey string
	a := &anthropicCompatible{client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		gotHost, gotPath, gotCookie = r.URL.Host, r.URL.Path, r.Header.Get("Cookie")
		gotAuth, gotXKey = r.Header.Get("Authorization"), r.Header.Get("x-api-key")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(claudePrepaidLiveBody)),
		}, nil
	})}}

	if _, err := a.ReadQuota(t.Context(), acct, "sk-ant-api"); err != ErrQuotaUnsupported || calls != 0 {
		t.Fatalf("no session: err=%v calls=%d", err, calls)
	}
	other := acct
	other.BaseURL = "https://api.minimax.io/anthropic"
	other.ManagementKey = "sessionKey=x; lastActiveOrg=" + claudeTestOrg
	if _, err := a.ReadQuota(t.Context(), other, "k"); err != ErrQuotaUnsupported || calls != 0 {
		t.Fatalf("non-anthropic host must not trigger the console read: err=%v calls=%d", err, calls)
	}

	acct.ManagementKey = "sessionKey=sk-ant-sid02-x; lastActiveOrg=" + claudeTestOrg
	q, err := a.ReadQuota(t.Context(), acct, "sk-ant-api")
	if err != nil || q.Kind != QuotaMoney {
		t.Fatalf("ReadQuota: kind=%q err=%v", q.Kind, err)
	}
	if gotHost != "platform.claude.com" || gotPath != "/api/organizations/"+claudeTestOrg+"/prepaid/credits" {
		t.Errorf("dialed %s%s", gotHost, gotPath)
	}
	if gotCookie != "sessionKey=sk-ant-sid02-x" {
		t.Errorf("cookie = %q", gotCookie)
	}
	if gotAuth != "" || gotXKey != "" {
		t.Errorf("inference key leaked to the console: auth=%q x-api-key=%q", gotAuth, gotXKey)
	}
}
