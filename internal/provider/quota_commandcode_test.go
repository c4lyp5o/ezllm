package provider

import (
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Verbatim live body, GET api.commandcode.ai/internal/billing/credits (2026-10-20).
const commandCodeLiveBody = `{"credits":{"belowThreshold":false,"creditThreshold":0,"monthlyCredits":45.7508028339,"purchasedCredits":0,"premiumMonthlyCredits":0,"opensourceMonthlyCredits":45.7508028339,"monthlyCreditsGranted":70},"windowLimits":{"limited":true,"exceeded":null,"fiveHour":{"used":0,"cap":14,"exceeded":false,"resetAt":0},"weekly":{"used":2.0799015087,"cap":35,"exceeded":false,"resetAt":1792986424541}},"sandboxAccess":true,"sandboxMinutes":{"usedMinutes":0,"limitMinutes":0,"overageUsd":0,"resetAt":1793491200000}}`

func TestParseCommandCodeCredits(t *testing.T) {
	q := parseCommandCodeCredits(commandCodeLiveBody)
	if q.Kind != QuotaPercent {
		t.Fatalf("kind = %q", q.Kind)
	}
	if q.PercentRolling == nil || *q.PercentRolling != 0 {
		t.Errorf("rolling = %v, want 0", q.PercentRolling)
	}
	if q.PercentWeekly == nil || *q.PercentWeekly < 5.94 || *q.PercentWeekly > 5.95 {
		t.Errorf("weekly = %v, want ~5.942", q.PercentWeekly)
	}
	if q.PercentMonthly == nil || *q.PercentMonthly < 34.64 || *q.PercentMonthly > 34.65 {
		t.Errorf("monthly = %v, want ~34.642", q.PercentMonthly)
	}
	if q.RollingResetsAt != "" {
		t.Errorf("idle 5h window must carry no reset, got %q", q.RollingResetsAt)
	}
	if q.WeeklyResetsAt != "2026-10-26T03:47:04Z" {
		t.Errorf("weekly reset = %q", q.WeeklyResetsAt)
	}
}

func TestParseCommandCodeCreditsGarbage(t *testing.T) {
	for _, body := range []string{``, `<html>`, `{}`, `{"error":"unauthorized"}`, `{"credits":{}}`} {
		if q := parseCommandCodeCredits(body); q.Kind != "" {
			t.Errorf("%q → kind %q, want zero Quota", body, q.Kind)
		}
	}
}

func TestCommandCodeCookieHeader(t *testing.T) {
	const name = "__Secure-commandcode_prod_.session_token"
	cases := map[string]string{
		"":                                       "",
		"abc.def%3D":                             name + "=abc.def%3D",
		name + "=abc.def%3D":                     name + "=abc.def%3D",
		"_ga=1; " + name + "=zz; __stripe_mid=2": name + "=zz",
		"_ga=1; __stripe_mid=2":                  "",
	}
	for in, want := range cases {
		if got := commandCodeCookieHeader(in); got != want {
			t.Errorf("commandCodeCookieHeader(%q) = %q, want %q", in, got, want)
		}
	}
}

// ReadQuota must dial the origin-rooted billing path with the Cookie header
// and WITHOUT the inference key; with no cookie it makes no call at all.
func TestReadQuotaCommandCode(t *testing.T) {
	acct := Account{Kind: KindOpenAICompatible, BaseURL: "https://api.commandcode.ai/provider/v1"}
	var calls int
	var gotPath, gotCookie, gotAuth string
	a := &openAICompatible{client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		gotPath, gotCookie, gotAuth = r.URL.Path, r.Header.Get("Cookie"), r.Header.Get("Authorization")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(commandCodeLiveBody)),
		}, nil
	})}}

	if _, err := a.ReadQuota(t.Context(), acct, "user_inference_key"); err != ErrQuotaUnsupported || calls != 0 {
		t.Fatalf("no cookie: err=%v calls=%d, want ErrQuotaUnsupported and no call", err, calls)
	}

	acct.ManagementKey = "tok%3D"
	q, err := a.ReadQuota(t.Context(), acct, "user_inference_key")
	if err != nil || q.Kind != QuotaPercent {
		t.Fatalf("ReadQuota: kind=%q err=%v", q.Kind, err)
	}
	if gotPath != "/internal/billing/credits" {
		t.Errorf("dialed %q", gotPath)
	}
	if gotCookie != "__Secure-commandcode_prod_.session_token=tok%3D" {
		t.Errorf("cookie = %q", gotCookie)
	}
	if gotAuth != "" {
		t.Errorf("inference key leaked to the billing endpoint: %q", gotAuth)
	}
}

// session_data is base64(JSON) with the expiry at session.session.expiresAt
// (shape taken from a live cookie; values synthetic).
func commandCodeSessionData(expiresAt string) string {
	j := `{"session":{"session":{"id":"x","expiresAt":"` + expiresAt + `"},"user":{"name":"u"}},"expiresAt":1792508722135,"signature":"s"}`
	return strings.TrimRight(base64.StdEncoding.EncodeToString([]byte(j)), "=")
}

func TestCommandCodeCookieExpiry(t *testing.T) {
	full := "_ga=1; __Secure-commandcode_prod_.session_token=tok%3D; __Secure-commandcode_prod_.session_data=" +
		commandCodeSessionData("2026-10-27T15:00:19.970Z") + "; __stripe_mid=2"
	got, ok := CommandCodeCookieExpiry(full)
	if !ok || got.UTC().Format(time.RFC3339) != "2026-10-27T15:00:19Z" {
		t.Fatalf("expiry = %v, %v", got, ok)
	}
	for _, in := range []string{"", "tok%3D", "__Secure-commandcode_prod_.session_token=tok", "__Secure-commandcode_prod_.session_data=!!!", "__Secure-commandcode_prod_.session_data=" + commandCodeSessionData("nope")} {
		if _, ok := CommandCodeCookieExpiry(in); ok {
			t.Errorf("%q: want no expiry", in)
		}
	}
}
