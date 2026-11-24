package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The claude-bridge account is anthropic-compatible on a loopback base; its
// plan windows live on {base}/usage (internal/claudebridge/limits.go).
func TestReadQuotaReadsTheLoopbackBridgeWindows(t *testing.T) {
	var gotPath, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotKey = r.URL.Path, r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"usage":{"rolling":{"percent":1,"resetsAt":"2026-10-23T07:40:00Z"},"weekly":{"percent":10,"resetsAt":"2026-10-25T00:00:00Z"}},"source":"claude-cli rate_limit_event","observed_at":"2026-10-23T03:00:00Z"}`))
	}))
	defer srv.Close()

	a := &anthropicCompatible{client: srv.Client()}
	q, err := a.ReadQuota(context.Background(), Account{Kind: KindAnthropicCompat, BaseURL: srv.URL + "/v1"}, "bridge-token")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/usage" || gotKey != "bridge-token" {
		t.Fatalf("read %s with x-api-key %q, want /v1/usage with the account key", gotPath, gotKey)
	}
	if q.Kind != QuotaPercent || q.PercentRolling == nil || *q.PercentRolling != 1 ||
		q.PercentWeekly == nil || *q.PercentWeekly != 10 || q.WeeklyResetsAt != "2026-10-25T00:00:00Z" {
		t.Fatalf("quota = %+v, want rolling 1%% / weekly 10%%", q)
	}
}

// Before the first call the bridge answers 404: the card stays "measured".
func TestReadQuotaBridgeWithoutAReadingIsUnsupported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	a := &anthropicCompatible{client: srv.Client()}
	if _, err := a.ReadQuota(context.Background(), Account{Kind: KindAnthropicCompat, BaseURL: srv.URL + "/v1"}, "k"); !errors.Is(err, ErrQuotaUnsupported) {
		t.Fatalf("err = %v, want ErrQuotaUnsupported", err)
	}
}

func TestIsLoopbackBase(t *testing.T) {
	for base, want := range map[string]bool{
		"http://127.0.0.1:8790/v1":  true,
		"http://localhost:8791/v1":  true,
		"http://[::1]:8790":         true,
		"https://api.anthropic.com": false,
		"https://127.0.0.1.nip.io":  false,
		"http://10.0.0.5:8790/v1":   false,
		"":                          false,
	} {
		if got := isLoopbackBase(base); got != want {
			t.Errorf("isLoopbackBase(%q) = %v, want %v", base, got, want)
		}
	}
}

// The codex-bridge account is openai-compatible on a loopback base; the
// generic {base}/usage read classifies its percent body with no adapter change.
func TestReadQuotaReadsTheCodexBridgeWindows(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/usage" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"observed_at":"2026-10-23T03:44:53Z","plan":"plus","source":"codex app-server account/rateLimits/read","usage":{"rolling":{"percent":2,"resetsAt":"2026-10-23T07:13:15Z","windowMinutes":300},"weekly":{"percent":0,"resetsAt":"2026-10-30T02:13:15Z","windowMinutes":10080}}}`))
	}))
	defer srv.Close()
	a := &openAICompatible{client: srv.Client()}
	q, err := a.ReadQuota(context.Background(), Account{Kind: KindOpenAICompatible, BaseURL: srv.URL + "/v1"}, "bridge-token")
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer bridge-token" || q.Kind != QuotaPercent || q.PercentRolling == nil || *q.PercentRolling != 2 ||
		q.PercentWeekly == nil || *q.PercentWeekly != 0 || q.RollingResetsAt != "2026-10-23T07:13:15Z" {
		t.Fatalf("auth=%q quota=%+v", gotAuth, q)
	}
}

// The agy bridge reports one window for the account and every group by name.
func TestQuotaGroups(t *testing.T) {
	const body = `{"usage":{"weekly":{"percent":10.56,"resetsAt":"2026-12-01T02:30:00Z"}},"groups":[{"name":"Gemini Models","window":"weekly","percent":10.56,"resets_at":"2026-12-01T02:30:00Z"},{"name":"Claude and GPT models","window":"weekly","percent":0}]}`
	q := ClassifyQuota(body)
	if q.Kind != QuotaPercent || q.PercentWeekly == nil || *q.PercentWeekly != 10.56 || q.PercentRolling != nil || q.Raw != body {
		t.Fatalf("quota = %+v", q)
	}
	got := QuotaGroups(q.Raw)
	if len(got) != 2 || got[0] != (QuotaGroup{"Gemini Models", 10.56, "2026-12-01T02:30:00Z"}) || got[1] != (QuotaGroup{"Claude and GPT models", 0, ""}) {
		t.Fatalf("groups = %+v", got)
	}
	for _, raw := range []string{"", "not json", `{"usage":{"weekly":{"percent":3}}}`, `{"groups":"x"}`, `{"groups":[]}`, `{"groups":[{"name":"","percent":1},{"name":"unknown"}]}`} {
		if got := QuotaGroups(raw); len(got) != 0 {
			t.Errorf("QuotaGroups(%q) = %+v, want none", raw, got)
		}
	}
}
