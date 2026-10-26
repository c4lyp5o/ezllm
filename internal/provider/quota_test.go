package provider

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────
// Host matrix for quotaOracleFor. Every URL below was live-probed 2026-10-14
// (docs/quota-oracles-design.md) — the expected strings are that evidence.
// ─────────────────────────────────────────────────────────────────────────────

func TestQuotaOracleHostMatrix(t *testing.T) {
	cases := []struct {
		name string
		acct Account
		ok   bool
		want string // prefix match against the resolved URL
	}{
		{"deepseek live base", Account{Kind: KindOpenAICompatible, BaseURL: "https://api.deepseek.com"}, true, "https://api.deepseek.com/user/balance"},
		{"deepseek trailing slash", Account{Kind: KindOpenAICompatible, BaseURL: "https://api.deepseek.com/"}, true, "https://api.deepseek.com/user/balance"},
		{"openrouter live base", Account{Kind: KindOpenAICompatible, BaseURL: "https://openrouter.ai/api/v1"}, true, "https://openrouter.ai/api/v1/key"},
		{"vercel live base", Account{Kind: KindOpenAICompatible, BaseURL: "https://ai-gateway.vercel.sh/v1"}, true, "https://ai-gateway.vercel.sh/v1/credits"},
		// Origin-rooted: appending to the /v1 base would 404.
		{"ollama live base", Account{Kind: KindOpenAICompatible, BaseURL: "https://ollama.com/v1"}, true, "https://ollama.com/api/usage?range=24h"},
		// Scoping: only https + exact host + openai-compatible may trigger.
		{"http scheme refused", Account{Kind: KindOpenAICompatible, BaseURL: "http://api.deepseek.com"}, false, ""},
		{"wrong kind refused", Account{Kind: KindAnthropicCompat, BaseURL: "https://api.deepseek.com"}, false, ""},
		{"name spoof refused", Account{Kind: KindOpenAICompatible, Name: "deepseek", BaseURL: "https://evil.example/v1"}, false, ""},
		{"namespace spoof refused", Account{Kind: KindOpenAICompatible, Namespace: "deepseek", BaseURL: "https://evil.example/v1"}, false, ""},
		{"host suffix refused", Account{Kind: KindOpenAICompatible, BaseURL: "https://notopenrouter.ai/api/v1"}, false, ""},
		{"host prefix refused", Account{Kind: KindOpenAICompatible, BaseURL: "https://openrouter.ai.evil.example/api/v1"}, false, ""},
		{"path spoof refused", Account{Kind: KindOpenAICompatible, BaseURL: "https://evil.example/api.deepseek.com/user/balance"}, false, ""},
		{"generic provider falls through", Account{Kind: KindOpenAICompatible, BaseURL: "https://space.stationine.com/v1"}, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotURL, parse, ok := quotaOracleFor(tc.acct)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (url=%q)", ok, tc.ok, gotURL)
			}
			if !tc.ok {
				if parse != nil {
					t.Errorf("parser must be nil when ok=false")
				}
				return
			}
			if !strings.HasPrefix(gotURL, tc.want) {
				t.Errorf("url = %q, want prefix %q", gotURL, tc.want)
			}
			if parse == nil {
				t.Errorf("parser is nil for a resolved oracle")
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Parsers — fixtures are VERBATIM live bodies captured 2026-10-14 with the
// operator's keys (self-consistent mocks prove nothing; these are vendor wire).
// ─────────────────────────────────────────────────────────────────────────────

func TestParseDeepSeekBalance(t *testing.T) {
	// Observed BOTH orders live (CNY-first, then USD-first) — USD must win
	// regardless.
	body := `{"is_available":true,"balance_infos":[` +
		`{"currency":"CNY","total_balance":"9.17","granted_balance":"0.00","topped_up_balance":"9.17"},` +
		`{"currency":"USD","total_balance":"0.09","granted_balance":"0.00","topped_up_balance":"0.09"}]}`
	q := parseDeepSeekBalance(body)
	if q.Kind != QuotaMoney {
		t.Fatalf("kind = %q, want money", q.Kind)
	}
	if q.Balance == nil || *q.Balance != 0.09 {
		t.Errorf("balance = %v, want USD 0.09 (USD preferred regardless of order)", q.Balance)
	}
	if q.BalanceUnit != "USD" {
		t.Errorf("unit = %q, want USD", q.BalanceUnit)
	}
	if q.Raw != body {
		t.Errorf("raw must keep the verbatim body")
	}
	// No USD entry → first entry is honest, unit records which one.
	q2 := parseDeepSeekBalance(`{"is_available":false,"balance_infos":[{"currency":"CNY","total_balance":"9.17"}]}`)
	if q2.Balance == nil || *q2.Balance != 9.17 || q2.BalanceUnit != "CNY" {
		t.Errorf("fallback pick = %v %q, want 9.17 CNY", q2.Balance, q2.BalanceUnit)
	}
	// is_available:false still reports the (possibly zero) balance — the UI
	// shows the number; raw keeps is_available for the modal.
	q3 := parseDeepSeekBalance(`{"is_available":false,"balance_infos":[{"currency":"USD","total_balance":"0.00"}]}`)
	if q3.Kind != QuotaMoney || q3.Balance == nil || *q3.Balance != 0.0 {
		t.Errorf("zero balance must still be money kind, got kind=%q bal=%v", q3.Kind, q3.Balance)
	}
}

func TestParseDeepSeekRejectsForeignShapes(t *testing.T) {
	for _, payload := range []string{
		`not json`,
		`{"balance_infos":[]}`,
		`{"balance_infos":[{"total_balance":"0.09"}]}`, // no currency
		`{"balance_infos":[{"currency":"USD","total_balance":"abc"}]}`,
		`{"data":[]}`,
	} {
		if q := parseDeepSeekBalance(payload); q.Kind != "" {
			t.Errorf("payload %q should be rejected, got %+v", payload, q.Kind)
		}
	}
}

func TestParseOpenRouterKey(t *testing.T) {
	// Verbatim /api/v1/key body (truncated only in unused fields).
	body := `{"data":{"label":"sk-or-v1-4fa...2a0","limit":1000,"limit_reset":null,` +
		`"limit_remaining":999.98323085,"usage":0.01676915,"usage_daily":0,` +
		`"usage_weekly":0.01676915,"usage_monthly":0.01676915,"is_free_tier":true}}`
	q := parseOpenRouterKey(body)
	if q.Kind != QuotaMoney {
		t.Fatalf("kind = %q, want money", q.Kind)
	}
	if q.Balance == nil || *q.Balance != 999.98323085 {
		t.Errorf("balance = %v, want 999.98323085", q.Balance)
	}
	if q.BalanceUnit != "USD" {
		t.Errorf("unit = %q, want USD", q.BalanceUnit)
	}
	if q.CostTotal == nil || *q.CostTotal != 0.01676915 {
		t.Errorf("cost_total = %v, want 0.01676915", q.CostTotal)
	}
	if q.CostToday == nil || *q.CostToday != 0 {
		t.Errorf("cost_today = %v, want 0", q.CostToday)
	}
	// limit_reset deliberately NOT mapped anywhere: it is an INTERVAL NAME
	// ("monthly"|null), not a timestamp — it stays in raw, not in any column.

	// Unlimited key: no limit_remaining but real usage → money with nil
	// balance (UI shows "–"), never a fabricated zero.
	q2 := parseOpenRouterKey(`{"data":{"limit":null,"limit_remaining":null,"usage":1.5,"usage_daily":0.25}}`)
	if q2.Kind != QuotaMoney || q2.Balance != nil {
		t.Errorf("unlimited key: kind=%q balance=%v, want money+nil", q2.Kind, q2.Balance)
	}
	if q2.CostTotal == nil || *q2.CostTotal != 1.5 {
		t.Errorf("unlimited key cost_total = %v, want 1.5", q2.CostTotal)
	}
}

func TestParseOpenRouterRejectsForeignShapes(t *testing.T) {
	for _, payload := range []string{`not json`, `{}`, `{"data":null}`, `{"data":{}}`, `{"other":1}`} {
		if q := parseOpenRouterKey(payload); q.Kind != "" {
			t.Errorf("payload %q should be rejected, got %+v", payload, q.Kind)
		}
	}
}

func TestParseVercelCredits(t *testing.T) {
	// Verbatim: BOTH values are JSON strings — jsonFloat would reject them.
	body := `{"balance":"4.3014513844","total_used":"0.6985486156"}`
	q := parseVercelCredits(body)
	if q.Kind != QuotaMoney {
		t.Fatalf("kind = %q, want money", q.Kind)
	}
	if q.Balance == nil || *q.Balance != 4.3014513844 {
		t.Errorf("balance = %v, want 4.3014513844 parsed from string", q.Balance)
	}
	if q.CostTotal == nil || *q.CostTotal != 0.6985486156 {
		t.Errorf("cost_total = %v, want 0.6985486156", q.CostTotal)
	}
	// Numeric (non-string) values must also parse — the vendor can change
	// their serializer without changing the contract.
	q2 := parseVercelCredits(`{"balance":2.5,"total_used":1.25}`)
	if q2.Balance == nil || *q2.Balance != 2.5 {
		t.Errorf("numeric balance = %v, want 2.5", q2.Balance)
	}
	// No balance = not this shape (nothing to show) → rejected.
	if q3 := parseVercelCredits(`{"total_used":"1"}`); q3.Kind != "" {
		t.Errorf("balance-less payload should be rejected, got %+v", q3.Kind)
	}
	if q4 := parseVercelCredits(`not json`); q4.Kind != "" {
		t.Errorf("garbage should be rejected, got %+v", q4.Kind)
	}
}

func TestParseOllamaUsage(t *testing.T) {
	// Verbatim /api/usage?range=24h body (buckets shortened — they ride raw).
	body := `{"range":"24h","scope":"self","granularity":"hour",` +
		`"from":"2026-10-13T07:00:00Z","until":"2026-10-14T07:04:00Z",` +
		`"totals":{"request_count":11},"buckets":[{"from":"x","until":"y","request_count":6}]}`
	q := parseOllamaUsage(body)
	if q.Kind != QuotaTokens {
		t.Fatalf("kind = %q, want tokens", q.Kind)
	}
	if q.ReqsToday == nil || *q.ReqsToday != 11 {
		t.Errorf("requests_today = %v, want 11", q.ReqsToday)
	}
	if q.TokensToday != nil {
		t.Errorf("tokens_today = %v, want nil (Ollama reports no token counts)", q.TokensToday)
	}
	if q.PercentRolling != nil || q.PercentMonthly != nil || q.PercentWeekly != nil {
		t.Errorf("no percent may be fabricated when the provider states no ceiling")
	}
	if !strings.Contains(q.Raw, `"buckets"`) {
		t.Errorf("raw must retain the buckets for the modal")
	}
	// A genuinely empty day stays 0 — a zero must SURVIVE, not become nil.
	q2 := parseOllamaUsage(`{"range":"24h","totals":{"request_count":0}}`)
	if q2.ReqsToday == nil || *q2.ReqsToday != 0 {
		t.Errorf("zero count = %v, want pointer-to-0", q2.ReqsToday)
	}
	for _, payload := range []string{`not json`, `{}`, `{"totals":null}`} {
		if q := parseOllamaUsage(payload); q.Kind != "" {
			t.Errorf("payload %q should be rejected, got %+v", payload, q.Kind)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
func TestMapOracleErr(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusMethodNotAllowed} {
		err := mapOracleErr(&HTTPError{Status: status, URL: "u", Body: "x"})
		if !errors.Is(err, ErrQuotaUnsupported) {
			t.Errorf("status %d: got %v, want ErrQuotaUnsupported", status, err)
		}
	}
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusInternalServerError} {
		err := mapOracleErr(&HTTPError{Status: status, URL: "u", Body: "x"})
		if errors.Is(err, ErrQuotaUnsupported) {
			t.Errorf("status %d must NOT map to unsupported: %v", status, err)
		}
		he, ok := err.(*HTTPError)
		if !ok || he.Status != status {
			t.Errorf("status %d must propagate the original error, got %v", status, err)
		}
	}
	// Non-HTTP errors (transport) propagate untouched.
	if err := mapOracleErr(errors.New("boom")); err.Error() != "boom" {
		t.Errorf("transport error altered: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// UA policy (Calypso's rule): ezllm's OWN requests carry agentUserAgent;
// PrepareRequest fills it only when absent so the client's UA and operator
// custom_headers both survive.
// ─────────────────────────────────────────────────────────────────────────────

func TestGetJSONStatusSendsAgentUA(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	if _, _, err := getJSONStatus(t.Context(), srv.Client(), srv.URL, nil); err != nil {
		t.Fatal(err)
	}
	if gotUA != agentUserAgent {
		t.Errorf("getJSON UA = %q, want %q (never a Go library default)", gotUA, agentUserAgent)
	}

	// An explicit caller UA wins.
	gotUA = ""
	if _, _, err := getJSONStatus(t.Context(), srv.Client(), srv.URL, map[string]string{"User-Agent": "custom/1"}); err != nil {
		t.Fatal(err)
	}
	if gotUA != "custom/1" {
		t.Errorf("caller UA = %q, want custom/1", gotUA)
	}
}

func TestPrepareRequestUAFillsOnlyWhenAbsent(t *testing.T) {
	oa := NewOpenAICompatible(http.DefaultClient)
	an := NewAnthropicCompatible(http.DefaultClient)
	acct := Account{Kind: KindOpenAICompatible, BaseURL: "https://x.example/v1"}

	// No UA → ours (the probe/quota/proxy-without-client-UA case).
	for _, tc := range []struct {
		name string
		run  func(*http.Request) error
	}{
		{"openai", func(r *http.Request) error { return oa.PrepareRequest(t.Context(), r, acct, "k", SurfaceChat) }},
		{"anthropic", func(r *http.Request) error { return an.PrepareRequest(t.Context(), r, acct, "k", SurfaceAnthropic) }},
	} {
		req, _ := http.NewRequest(http.MethodPost, "https://x.example", nil)
		if err := tc.run(req); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := req.Header.Get("User-Agent"); got != agentUserAgent {
			t.Errorf("%s no-UA: got %q, want %q", tc.name, got, agentUserAgent)
		}
	}

	// Client UA present → preserved (inference passthrough contract).
	req, _ := http.NewRequest(http.MethodPost, "https://x.example", nil)
	req.Header.Set("User-Agent", "client-app/9.9")
	if err := oa.PrepareRequest(t.Context(), req, acct, "k", SurfaceChat); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("User-Agent"); got != "client-app/9.9" {
		t.Errorf("client UA overwritten: %q", got)
	}

	// Operator custom_headers UA wins over ours (applied after).
	acct.CustomHeaders = map[string]string{"User-Agent": "operator/2"}
	req, _ = http.NewRequest(http.MethodPost, "https://x.example", nil)
	if err := oa.PrepareRequest(t.Context(), req, acct, "k", SurfaceChat); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("User-Agent"); got != "operator/2" {
		t.Errorf("custom header UA = %q, want operator/2", got)
	}
}

// roundTripFunc lets a test intercept the adapter's DIALED request — this is
// how ReadQuota (which resolves absolute production URLs) can be driven in
// package tests without touching the network.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestReadQuotaDispatch drives openAICompatible.ReadQuota ITSELF (the real
// dispatch: quotaOracleFor → getJSON → parse). The transport serves the canned
// body ONLY on the oracle's exact path; every other URL (notably the generic
// {base}/usage fallback) answers 404 — so a ReadQuota that stops consulting
// the oracle table fails here, not just the helpers around it.
func TestReadQuotaDispatch(t *testing.T) {
	canned := map[string]string{
		// Full paths as REALLY dialed (base + oracle path — the openrouter and
		// vercel oracle paths carry their base's version prefix).
		"/api/v1/key": `{"data":{"limit":1000,"limit_remaining":999.5,"usage":0.5,"usage_daily":0.1}}`,
		"/v1/credits": `{"balance":"4.30","total_used":"0.70"}`,
		"/user/balance": `{"is_available":true,"balance_infos":[` +
			`{"currency":"CNY","total_balance":"9.17"},{"currency":"USD","total_balance":"0.09"}]}`,
		"/api/usage": `{"range":"24h","totals":{"request_count":11}}`,
	}
	cases := []struct {
		name      string
		account   Account
		wantPath  string // exact path ReadQuota must dial
		wantKind  QuotaKind
		wantUni   string
		wantCount *int64
	}{
		{"deepseek", Account{Kind: KindOpenAICompatible, BaseURL: "https://api.deepseek.com"}, "/user/balance", QuotaMoney, "USD", nil},
		{"openrouter", Account{Kind: KindOpenAICompatible, BaseURL: "https://openrouter.ai/api/v1"}, "/api/v1/key", QuotaMoney, "USD", nil},
		{"vercel", Account{Kind: KindOpenAICompatible, BaseURL: "https://ai-gateway.vercel.sh/v1"}, "/v1/credits", QuotaMoney, "USD", nil},
		{"ollama", Account{Kind: KindOpenAICompatible, BaseURL: "https://ollama.com/v1"}, "/api/usage", QuotaTokens, "", ptr(int64(11))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var dialed, gotUA, gotAuth string
			a := &openAICompatible{client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				dialed = r.URL.Path
				gotUA = r.Header.Get("User-Agent")
				gotAuth = r.Header.Get("Authorization")
				body, ok := canned[r.URL.Path]
				code := http.StatusOK
				if !ok {
					code = http.StatusNotFound
					body = `{"error":"wrong path"}` // proves ReadQuota asked the fallback, not the oracle
				}
				return &http.Response{
					StatusCode: code,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(body)),
				}, nil
			})}}

			q, err := a.ReadQuota(t.Context(), tc.account, "sk-live")
			if err != nil {
				t.Fatalf("ReadQuota: %v (dialed %q — dispatch missed the oracle)", err, dialed)
			}
			if dialed != tc.wantPath {
				t.Errorf("dialed %q, want %q", dialed, tc.wantPath)
			}
			if q.Kind != tc.wantKind {
				t.Errorf("kind = %q, want %q", q.Kind, tc.wantKind)
			}
			if tc.wantUni != "" && (q.BalanceUnit != tc.wantUni || q.Balance == nil) {
				t.Errorf("balance = %v %q, want non-nil %q", q.Balance, q.BalanceUnit, tc.wantUni)
			}
			if tc.wantCount != nil && (q.ReqsToday == nil || *q.ReqsToday != *tc.wantCount) {
				t.Errorf("requests_today = %v, want %d", q.ReqsToday, *tc.wantCount)
			}
			if gotAuth != "Bearer sk-live" {
				t.Errorf("auth = %q, want Bearer sk-live", gotAuth)
			}
			if gotUA != agentUserAgent {
				t.Errorf("UA = %q, want %q", gotUA, agentUserAgent)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

// A provider with a GENERIC shape (no oracle) still walks the {base}/usage
// fallback — the oracle table must not swallow unrelated hosts.
func TestReadQuotaFallsBackToUsagePath(t *testing.T) {
	var dialed string
	a := &openAICompatible{client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		dialed = r.URL.String()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"usage":{"rolling":{"percent":7.0,"resetsAt":"t"},"weekly":{"percent":12.0},"monthly":{"percent":30.0}}}`)),
		}, nil
	})}}
	q, err := a.ReadQuota(t.Context(), Account{Kind: KindOpenAICompatible, BaseURL: "https://opencode.example/zen/go/v1"}, "k")
	if err != nil {
		t.Fatalf("ReadQuota: %v", err)
	}
	if dialed != "https://opencode.example/zen/go/v1/usage" {
		t.Errorf("dialed %q, want the generic /usage fallback", dialed)
	}
	if q.Kind != QuotaPercent {
		t.Errorf("kind = %q, want percent", q.Kind)
	}
}
