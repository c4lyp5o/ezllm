package claudebridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// limitExec reports the rate_limit_info shape measured live 2026-10-23.
type limitExec struct{ info map[string]any }

func (l *limitExec) Run(_ context.Context, _ Request, _ string, _ func(Frame) error) (Result, error) {
	return Result{RateLimit: l.info}, nil
}

func measuredRateLimit() map[string]any {
	var m map[string]any
	_ = json.Unmarshal([]byte(`{"status":"allowed","resetsAt":1792741200,"rateLimitType":"five_hour",
		"unifiedWindows":{"five_hour":{"utilization":0.01,"resetsAt":1792741200},
		"seven_day":{"utilization":0.1,"resetsAt":1792886400}}}`), &m)
	return m
}

func getUsage(t *testing.T, s *Server, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	if key != "" {
		req.Header.Set("x-api-key", key)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestUsageServesTheLastPlanWindowsAsPercent(t *testing.T) {
	s := testServer(&limitExec{info: measuredRateLimit()})
	s.Limits = &Limits{}

	if rec := getUsage(t, s, "secret"); rec.Code != http.StatusNotFound {
		t.Fatalf("before any call: status %d, want 404 (no reading yet)", rec.Code)
	}
	if rec := post(t, s, `{"model":"claude-haiku-5-5","messages":[{"role":"user","content":"hi"}]}`); rec.Code != http.StatusOK {
		t.Fatalf("call failed: %d %s", rec.Code, rec.Body)
	}
	rec := getUsage(t, s, "secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("after a call: status %d %s", rec.Code, rec.Body)
	}
	var got struct {
		Usage map[string]struct {
			Percent  float64 `json:"percent"`
			ResetsAt string  `json:"resetsAt"`
		} `json:"usage"`
		ObservedAt string `json:"observed_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if r := got.Usage["rolling"]; r.Percent != 1 || r.ResetsAt != "2026-10-23T07:40:00Z" {
		t.Fatalf("rolling = %+v, want 1%% resetting 2026-10-23T07:40:00Z (fraction ×100)", r)
	}
	if w := got.Usage["weekly"]; w.Percent != 10 || w.ResetsAt != "2026-10-25T00:00:00Z" {
		t.Fatalf("weekly = %+v, want 10%%", w)
	}
	if got.ObservedAt == "" {
		t.Fatal("observed_at missing: the card must be able to say how fresh the reading is")
	}
}

func TestUsageRequiresTheSharedSecret(t *testing.T) {
	s := testServer(&limitExec{})
	s.Limits = &Limits{}
	if rec := getUsage(t, s, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key: status %d, want 401", rec.Code)
	}
}

func TestARunWithoutRateLimitKeepsThePreviousReading(t *testing.T) {
	l := &Limits{}
	l.Observe(measuredRateLimit(), time.Unix(1792720000, 0))
	l.Observe(nil, time.Unix(1792720000, 0))
	l.Observe(map[string]any{"status": "allowed"}, time.Unix(1792720000, 0))
	if _, ok := l.UsageBody(); !ok {
		t.Fatal("an empty or window-less rate_limit_info wiped the last reading")
	}
}
