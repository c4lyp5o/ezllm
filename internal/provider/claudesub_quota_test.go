package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/claudeweb"
)

const claudeSubUsageBody = `{"five_hour":{"utilization":1.0,"resets_at":"2026-10-26T07:30:00.182303+00:00"},"seven_day":{"utilization":93.0,"resets_at":"2026-10-29T01:00:00.182325+00:00"},"seven_day_opus":null}`

const claudeSubCookie = "anthropic-device-id=dev-1; lastActiveOrg=3e47a76e-9914-4ad3-aa0f-2143df5926a6; sessionKey=sk-ant-sid02-x; cf_clearance=cf"

type fakeClaudeFetcher struct {
	status int
	body   string
	err    error
	url    string
	cookie string
}

func (f *fakeClaudeFetcher) Fetch(_ context.Context, _, url string, _ []byte, h http.Header) (*claudeweb.Response, error) {
	f.url, f.cookie = url, h.Get("Cookie")
	if f.err != nil {
		return nil, f.err
	}
	return &claudeweb.Response{Status: f.status, Body: io.NopCloser(strings.NewReader(f.body))}, nil
}

func bridgeServing(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestParseClaudeSubUsage(t *testing.T) {
	q := parseClaudeSubUsage(claudeSubUsageBody)
	if q.Kind != QuotaPercent || q.PercentRolling == nil || *q.PercentRolling != 1 ||
		q.PercentWeekly == nil || *q.PercentWeekly != 93 ||
		q.RollingResetsAt != "2026-10-26T07:30:00Z" || q.WeeklyResetsAt != "2026-10-29T01:00:00Z" {
		t.Fatalf("quota = %+v", q)
	}
	if got := parseClaudeSubUsage(`{"five_hour":null,"seven_day":null}`); got.Kind != "" {
		t.Fatalf("empty windows parsed as %+v", got)
	}
}

func TestClaudeSubCreds(t *testing.T) {
	if _, org, ok := claudeSubCreds(claudeSubCookie); !ok || org != "3e47a76e-9914-4ad3-aa0f-2143df5926a6" {
		t.Fatalf("creds ok=%v org=%q", ok, org)
	}
	for _, bad := range []string{"", "sessionKey=sk-ant", "lastActiveOrg=3e47a76e-9914-4ad3-aa0f-2143df5926a6; x=1",
		"sessionKey=a; lastActiveOrg=../admin"} {
		if _, _, ok := claudeSubCreds(bad); ok {
			t.Errorf("claudeSubCreds(%q) accepted", bad)
		}
	}
}

func TestReadQuotaClaudeSubPrefersTheFreshUsageRead(t *testing.T) {
	bridge := bridgeServing(t, `{"usage":{"rolling":{"percent":50}}}`)
	f := &fakeClaudeFetcher{status: 200, body: claudeSubUsageBody}
	a := &anthropicCompatible{client: bridge.Client(), fetcher: f}
	q, err := a.ReadQuota(context.Background(), Account{Kind: KindAnthropicCompat, BaseURL: bridge.URL + "/v1", ManagementKey: claudeSubCookie}, "k")
	if err != nil {
		t.Fatal(err)
	}
	if f.url != "https://claude.ai/api/organizations/3e47a76e-9914-4ad3-aa0f-2143df5926a6/usage" || f.cookie != claudeSubCookie {
		t.Fatalf("fetched %q with cookie %q", f.url, f.cookie)
	}
	if q.PercentWeekly == nil || *q.PercentWeekly != 93 {
		t.Fatalf("quota = %+v, want the claude.ai weekly 93", q)
	}
	if !strings.Contains(q.Raw, `"source":"claude.ai usage"`) || strings.Contains(q.Raw, "live_read_error") {
		t.Fatalf("raw = %s, want the claude.ai source tag and no error", q.Raw)
	}
}

func TestReadQuotaClaudeSubFallsBackToTheBridgeReading(t *testing.T) {
	bridge := bridgeServing(t, `{"usage":{"rolling":{"percent":50}}}`)
	for name, f := range map[string]*fakeClaudeFetcher{
		"transport error": {err: errors.New("boom")},
		"challenge":       {status: 403, body: "Just a moment"},
		"bad body":        {status: 200, body: "{}"},
	} {
		a := &anthropicCompatible{client: bridge.Client(), fetcher: f}
		q, err := a.ReadQuota(context.Background(), Account{Kind: KindAnthropicCompat, BaseURL: bridge.URL + "/v1", ManagementKey: claudeSubCookie}, "k")
		if err != nil || q.PercentRolling == nil || *q.PercentRolling != 50 {
			t.Errorf("%s: quota=%+v err=%v, want the bridge's rolling 50", name, q, err)
		}
		if !strings.Contains(q.Raw, `"live_read_error"`) {
			t.Errorf("%s: raw = %s, want live_read_error naming why the live read was skipped", name, q.Raw)
		}
	}
}

func TestReadQuotaClaudeSubWithoutCookieNeverFetchesClaudeAI(t *testing.T) {
	bridge := bridgeServing(t, `{"usage":{"rolling":{"percent":50}}}`)
	f := &fakeClaudeFetcher{status: 200, body: claudeSubUsageBody}
	a := &anthropicCompatible{client: bridge.Client(), fetcher: f}
	if _, err := a.ReadQuota(context.Background(), Account{Kind: KindAnthropicCompat, BaseURL: bridge.URL + "/v1"}, "k"); err != nil {
		t.Fatal(err)
	}
	if f.url != "" {
		t.Fatalf("fetched %q without a cookie", f.url)
	}
	if q, _ := a.ReadQuota(context.Background(), Account{Kind: KindAnthropicCompat, BaseURL: bridge.URL + "/v1"}, "k"); strings.Contains(q.Raw, "live_read_error") {
		t.Fatalf("raw = %s: no cookie means no live read to report failing", q.Raw)
	}
}
