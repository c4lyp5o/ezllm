package server

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/store"
)

// withMetricsKey adds a key that holds only the metrics role.
func withMetricsKey(t *testing.T, h *harness) {
	t.Helper()
	if err := h.db.UpsertClientToken(context.Background(), "prometheus", testMetricsToken, "metrics"); err != nil {
		t.Fatal(err)
	}
}

const testMetricsToken = "tok-metrics"

func chatUpstream(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"id":"x","choices":[{"message":{"content":"OK"}}],"usage":{"prompt_tokens":2044,"completion_tokens":72,"total_tokens":2116,"prompt_tokens_details":{"cached_tokens":1792},"completion_tokens_details":{"reasoning_tokens":28}}}`))
}

func TestMetricsAuth(t *testing.T) {
	h := newHarness(t, chatUpstream)
	withMetricsKey(t, h)
	for _, c := range []struct {
		token string
		want  int
	}{
		{"", http.StatusUnauthorized},
		{"wrong-token-wrong-token", http.StatusUnauthorized},
		{"tok-infer", http.StatusForbidden}, // an inference key cannot read spend
		{"tok-admin", http.StatusForbidden}, // each role opens one surface
		{testMetricsToken, http.StatusOK},
	} {
		code, hdr, body := h.do("GET", "/metrics", c.token, "")
		if code != c.want {
			t.Errorf("token %q: %d, want %d", c.token, code, c.want)
		}
		if code == http.StatusOK && !strings.HasPrefix(hdr.Get("Content-Type"), "text/plain; version=0.0.4") {
			t.Errorf("content type %q", hdr.Get("Content-Type"))
		}
		if code != http.StatusOK && strings.Contains(body, "ezllm_requests_total") {
			t.Errorf("token %q: refused scrape leaked metrics", c.token)
		}
	}
}

// The dashboard session is the admin surface; it does not read /metrics.
func TestMetricsRefuseTheDashboardSession(t *testing.T) {
	h := newHarness(t, chatUpstream)
	setupDashboard(t, h, "metrics-test-password")
	sess, err := h.db.DashboardLogin(context.Background(), "metrics-test-password")
	if err != nil {
		t.Fatal(err)
	}
	if code, _, _ := h.do("GET", "/metrics", sess, ""); code != http.StatusUnauthorized {
		t.Fatalf("dashboard session (bearer): %d, want 401", code)
	}
	req := httptest.NewRequest("GET", "/metrics", nil)
	req.AddCookie(&http.Cookie{Name: "ezllm_session", Value: sess})
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("dashboard session (cookie): %d, want 401", rec.Code)
	}
}

// A metrics key reads /metrics and nothing else.
func TestMetricsKeyCannotInferOrAdmin(t *testing.T) {
	h := newHarness(t, chatUpstream)
	withMetricsKey(t, h)
	for _, p := range []struct{ m, path, body string }{
		{"POST", "/v1/chat/completions", `{"model":"up/gpt-6-luna","messages":[{"role":"user","content":"hi"}]}`},
		{"GET", "/v1/models", ""},
		{"GET", "/api/v1/vscode/" + testMetricsToken + "/models", ""},
		{"GET", "/admin/health", ""},
	} {
		tok := testMetricsToken
		if strings.HasPrefix(p.path, "/api/v1/vscode/") {
			tok = ""
		}
		if code, _, _ := h.do(p.m, p.path, tok, p.body); code != http.StatusForbidden {
			t.Errorf("metrics key on %s %s: %d, want 403", p.m, p.path, code)
		}
	}
}

// A real request reaches the scrape through the ledger, with the normalized
// accounting the ledger stored (cached tokens split out of prompt_tokens).
func TestMetricsCountCommittedRequests(t *testing.T) {
	h := newHarness(t, chatUpstream)
	withMetricsKey(t, h)
	for range 2 {
		if code, _, body := h.do("POST", "/v1/chat/completions", "tok-infer", `{"model":"up/gpt-6-luna","messages":[{"role":"user","content":"hi"}]}`); code != 200 {
			t.Fatalf("chat: %d %s", code, body)
		}
	}
	if err := h.db.Flush(); err != nil {
		t.Fatal(err)
	}
	_, _, body := h.do("GET", "/metrics", testMetricsToken, "")
	for _, want := range []string{
		`ezllm_requests_total{surface="chat",account="up",model="gpt-6-luna",status="200"} 2`,
		`ezllm_tokens_total{account="up",model="gpt-6-luna",kind="input"} 504`,
		`ezllm_tokens_total{account="up",model="gpt-6-luna",kind="output"} 144`,
		`ezllm_tokens_total{account="up",model="gpt-6-luna",kind="cached_read"} 3584`,
		`ezllm_tokens_total{account="up",model="gpt-6-luna",kind="reasoning"} 56`,
		`ezllm_request_duration_seconds_count{surface="chat",account="up"} 2`,
		`ezllm_request_duration_seconds_bucket{surface="chat",account="up",le="+Inf"} 2`,
		`ezllm_build_info{version="` + Version + `"} 1`,
		`ezllm_inflight_requests 0`,
		`ezllm_inflight_limit 32`,
		"# TYPE ezllm_request_duration_seconds histogram",
		"go_goroutines ",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape missing %q", want)
		}
	}
	if strings.Contains(body, "tok-infer") || strings.Contains(body, "hermes") {
		t.Error("scrape names a client token or client")
	}
}

// Scrapes race the ledger goroutine's observer; -race must stay quiet.
func TestMetricsScrapeDuringLedgerWrites(t *testing.T) {
	h := newHarness(t, chatUpstream)
	withMetricsKey(t, h)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ms := int64(10)
		for range 200 {
			h.db.RecordCall(store.Call{TS: time.Now(), Surface: store.SurfaceChat, Account: "up", Model: "gpt-6-luna", Status: 200, Totalms: &ms})
		}
	}()
	for range 20 {
		if code, _, _ := h.do("GET", "/metrics", testMetricsToken, ""); code != 200 {
			t.Fatalf("scrape %d", code)
		}
	}
	<-done
	if err := h.db.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, _, body := h.do("GET", "/metrics", testMetricsToken, ""); !strings.Contains(body, `status="200"} 200`) {
		t.Error("not every committed row was counted")
	}
}

func TestMetricsHistogramBuckets(t *testing.T) {
	m := newMetricsCollector(time.Now())
	ms := func(v int64) *int64 { return &v }
	m.observe([]store.Call{
		{Surface: store.SurfaceChat, Account: "a", Model: "m", Status: 200, Totalms: ms(80), TTFTms: ms(40)},
		{Surface: store.SurfaceChat, Account: "a", Model: "m", Status: 200, Totalms: ms(1000)},
		{Surface: store.SurfaceChat, Account: "a", Model: "m", Status: 502, Totalms: ms(400_000)},
	})
	var b strings.Builder
	m.write(&b)
	out := b.String()
	for _, want := range []string{
		`ezllm_request_duration_seconds_bucket{surface="chat",account="a",le="0.05"} 0`,
		`ezllm_request_duration_seconds_bucket{surface="chat",account="a",le="0.1"} 1`,
		`ezllm_request_duration_seconds_bucket{surface="chat",account="a",le="1"} 2`, // le is inclusive
		`ezllm_request_duration_seconds_bucket{surface="chat",account="a",le="300"} 2`,
		`ezllm_request_duration_seconds_bucket{surface="chat",account="a",le="+Inf"} 3`,
		`ezllm_request_duration_seconds_sum{surface="chat",account="a"} 401.08`,
		`ezllm_time_to_first_token_seconds_count{surface="chat",account="a"} 1`,
		`ezllm_requests_total{surface="chat",account="a",model="m",status="502"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}

// A caller cycling model names on refused requests must not grow the scrape
// without bound.
func TestMetricsCardinalityCap(t *testing.T) {
	m := newMetricsCollector(time.Now())
	rows := make([]store.Call, metricsSeriesCap+50)
	for i := range rows {
		rows[i] = store.Call{Surface: store.SurfaceChat, Account: "a", Model: fmt.Sprintf("junk-%d", i), Status: 404}
	}
	m.observe(rows)
	if len(m.requests) > metricsSeriesCap+1 || len(m.tokens) > metricsSeriesCap+1 {
		t.Fatalf("series grew past the cap: requests=%d tokens=%d", len(m.requests), len(m.tokens))
	}
	if m.requests[reqKey{"chat", "a", "_other", 404}] != 50 {
		t.Errorf("overflow not folded into _other: %d", m.requests[reqKey{"chat", "a", "_other", 404}])
	}
}

func TestMetricFloatFormat(t *testing.T) {
	for v, want := range map[float64]string{1794791636: "1794791636", 0: "0", 0.05: "0.05", 401.08: "401.08", math.Inf(1): "+Inf"} {
		if got := formatFloat(v); got != want {
			t.Errorf("formatFloat(%v) = %s, want %s", v, got, want)
		}
	}
}

func TestMetricLabelsEscape(t *testing.T) {
	got := metricLabels("model", "a\"b\\c\nd")
	if want := `model="a\"b\\c\nd"`; got != want {
		t.Errorf("metricLabels = %s, want %s", got, want)
	}
}

// The observer runs on the ledger goroutine; keep its per-row cost visible.
func BenchmarkMetricsObserve(b *testing.B) {
	m := newMetricsCollector(time.Now())
	ms := int64(1200)
	rows := make([]store.Call, 64)
	for i := range rows {
		rows[i] = store.Call{Surface: store.SurfaceChat, Account: "up", Model: "gpt-6-luna", Status: 200, Totalms: &ms, TTFTms: &ms, TokensIn: 100, TokensOut: 20}
	}
	b.ReportAllocs()
	for b.Loop() {
		m.observe(rows)
	}
}
