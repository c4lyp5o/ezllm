package provider

import (
	"net/http"
	"testing"
	"time"
)

func testAccount(kind Kind, base string) Account {
	return Account{
		ID: 1, Name: "test", Namespace: "test", Kind: kind,
		BaseURL: base, Enabled: true, ProbeDelay: 10 * time.Millisecond,
	}
}

// TestPrepareRequestInjectsSession proves the opencode-go quirk is handled:
// a missing x-opencode-session yields 400 MissingSessionID upstream.
func TestOpenCodeGoInjectsSessionHeaders(t *testing.T) {
	a := NewOpenCodeGo(http.DefaultClient)
	acct := testAccount(KindOpenCodeGo, "https://opencode.ai/zen/go/v1")
	acct.RequiresSessionHeader = true

	for _, surface := range []Surface{SurfaceOpenAI, SurfaceAnthropic, SurfaceResponses} {
		req, _ := http.NewRequest(http.MethodPost, acct.BaseURL+SurfacePath(surface), nil)
		// simulate a malicious/naive client pinning its own session
		req.Header.Set("x-opencode-session", "client-evil-session")
		req.Header.Set("x-opencode-request", "client-evil-req")
		if err := a.PrepareRequest(t.Context(), req, acct, "sk-test-key", surface); err != nil {
			t.Fatalf("surface %s: %v", surface, err)
		}
		sess := req.Header.Get("x-opencode-session")
		if sess == "" {
			t.Errorf("surface %s: x-opencode-session missing (upstream would 400)", surface)
		}
		if sess == "client-evil-session" {
			t.Errorf("surface %s: client-supplied session header was NOT replaced", surface)
		}
		if got := req.Header.Get("x-opencode-request"); got == "" || got == "client-evil-req" {
			t.Errorf("surface %s: request id not regenerated: %q", surface, got)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer sk-test-key" {
			t.Errorf("surface %s: Authorization = %q", surface, got)
		}
	}
}

// Anthropic surface must also carry x-api-key + anthropic-version (verified live).
func TestOpenCodeGoAnthropicSurfaceAuth(t *testing.T) {
	a := NewOpenCodeGo(http.DefaultClient)
	acct := testAccount(KindOpenCodeGo, "https://opencode.ai/zen/go/v1")
	req, _ := http.NewRequest(http.MethodPost, acct.BaseURL+"/messages", nil)
	if err := a.PrepareRequest(t.Context(), req, acct, "sk-k", SurfaceAnthropic); err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("x-api-key") != "sk-k" {
		t.Errorf("x-api-key = %q, want the key", req.Header.Get("x-api-key"))
	}
	if req.Header.Get("anthropic-version") == "" {
		t.Error("anthropic-version header missing")
	}
}

// Each request must get a DIFFERENT session id — a shared session grows
// server-side and eventually blows context.
func TestOpenCodeGoSessionIDsAreUnique(t *testing.T) {
	a := NewOpenCodeGo(http.DefaultClient)
	acct := testAccount(KindOpenCodeGo, "https://opencode.ai/zen/go/v1")
	acct.RequiresSessionHeader = true
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		req, _ := http.NewRequest(http.MethodPost, acct.BaseURL+"/chat/completions", nil)
		if err := a.PrepareRequest(t.Context(), req, acct, "sk-k", SurfaceOpenAI); err != nil {
			t.Fatal(err)
		}
		s := req.Header.Get("x-opencode-session")
		if seen[s] {
			t.Fatalf("session id reused after %d requests: %s", i, s)
		}
		seen[s] = true
	}
}

func TestPrepareRequestRejectsEmptyKey(t *testing.T) {
	acct := testAccount(KindOpenCodeGo, "https://x/v1")
	for _, a := range []Adapter{
		NewOpenCodeGo(http.DefaultClient),
		NewOpenAICompatible(http.DefaultClient),
		NewAnthropicCompatible(http.DefaultClient),
	} {
		req, _ := http.NewRequest(http.MethodPost, "https://x/v1/chat/completions", nil)
		if err := a.PrepareRequest(t.Context(), req, acct, "", SurfaceOpenAI); err == nil {
			t.Errorf("%s: empty key must be rejected", a.Kind())
		}
	}
}

// Anthropic-compatible must NOT send a Bearer header (that shape confuses
// Anthropic endpoints), and must strip a client-supplied one.
func TestAnthropicCompatibleAuthShape(t *testing.T) {
	a := NewAnthropicCompatible(http.DefaultClient)
	acct := testAccount(KindAnthropicCompat, "https://api.anthropic.com/v1")
	req, _ := http.NewRequest(http.MethodPost, acct.BaseURL+"/messages", nil)
	req.Header.Set("Authorization", "***")
	if err := a.PrepareRequest(t.Context(), req, acct, "sk-ant-x", SurfaceAnthropic); err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("x-api-key") != "sk-ant-x" {
		t.Error("x-api-key not set")
	}
	// StripHeaders must include Authorization so the proxy drops the client's
	stripped := false
	for _, h := range a.StripHeaders() {
		if h == "Authorization" {
			stripped = true
		}
	}
	if !stripped {
		t.Error("StripHeaders must include Authorization for anthropic-compatible")
	}
}

// opencode-go must strip client-supplied session headers.
func TestStripHeadersListed(t *testing.T) {
	a := NewOpenCodeGo(http.DefaultClient)
	want := map[string]bool{"X-Opencode-Session": false, "X-Opencode-Request": false}
	for _, h := range a.StripHeaders() {
		if _, ok := want[h]; ok {
			want[h] = true
		}
	}
	for h, found := range want {
		if !found {
			t.Errorf("StripHeaders missing %s", h)
		}
	}
}

func TestParseKindAndValidKinds(t *testing.T) {
	for _, good := range []string{"opencode-go", "openai-compatible", "anthropic-compatible", " opencode-go "} {
		if _, err := ParseKind(good); err != nil {
			t.Errorf("ParseKind(%q) failed: %v", good, err)
		}
	}
	for _, bad := range []string{"", "github-copilot", "opencode", "OPENAI-COMPATIBLE"} {
		if _, err := ParseKind(bad); err == nil {
			t.Errorf("ParseKind(%q) should fail", bad)
		}
	}
	if len(ValidKinds()) != 3 {
		t.Errorf("ValidKinds = %v, want 3 entries", ValidKinds())
	}
}

func TestSurfacePath(t *testing.T) {
	cases := map[Surface]string{
		SurfaceOpenAI:    "/chat/completions",
		SurfaceAnthropic: "/messages",
		SurfaceResponses: "/responses",
		Surface("weird"): "/chat/completions", // default
	}
	for s, want := range cases {
		if got := SurfacePath(s); got != want {
			t.Errorf("SurfacePath(%s) = %s, want %s", s, got, want)
		}
	}
}

func TestRegistryGetAndKinds(t *testing.T) {
	r := NewRegistry(http.DefaultClient)
	for _, k := range ValidKinds() {
		a, err := r.Get(k)
		if err != nil {
			t.Errorf("Get(%s): %v", k, err)
			continue
		}
		if a.Kind() != k {
			t.Errorf("adapter kind = %s, want %s", a.Kind(), k)
		}
	}
	if _, err := r.Get(Kind("nope")); err == nil {
		t.Error("Get(unknown) must fail")
	}
	if len(r.Kinds()) != 3 {
		t.Errorf("registry has %d kinds, want 3", len(r.Kinds()))
	}
}

// ── quota parsing against the REAL captured payloads (P10 / P17) ────────────

func TestQuotaParseOpenCodePercentShape(t *testing.T) {
	// Verbatim payload captured live from opencode-go on 2026-10-05.
	payload := `{"usage":{"rolling":{"status":"ok","percent":0,"resetsAt":"2026-10-05T15:24:51.000Z"},"weekly":{"status":"ok","percent":7,"resetsAt":"2026-10-12T00:00:00.000Z"},"monthly":{"status":"ok","percent":70,"resetsAt":"2026-10-13T14:36:35.000Z"}}}`
	q := parsePercentQuota(payload)
	if q.Kind != QuotaPercent {
		t.Fatalf("kind = %s, want percent", q.Kind)
	}
	if q.PercentWeekly == nil || *q.PercentWeekly != 7 {
		t.Errorf("weekly = %v, want 7", q.PercentWeekly)
	}
	if q.PercentMonthly == nil || *q.PercentMonthly != 70 {
		t.Errorf("monthly = %v, want 70", q.PercentMonthly)
	}
	if q.PercentRolling == nil || *q.PercentRolling != 0 {
		t.Errorf("rolling = %v, want 0", q.PercentRolling)
	}
	if q.WeeklyResetsAt != "2026-10-12T00:00:00.000Z" {
		t.Errorf("weekly resetsAt = %q", q.WeeklyResetsAt)
	}
	if q.Raw == "" {
		t.Error("raw payload must be retained for audit")
	}
}

func TestQuotaParseSSNMoneyShape(t *testing.T) {
	// Verbatim (abridged) payload captured live from ssn-gpt on 2026-10-05.
	payload := `{"balance":0.25667416,"remaining":0.25667416,"unit":"USD","isValid":true,"mode":"unrestricted","planName":"钱包余额","usage":{"rpm":0,"average_duration_ms":16463.24,"today":{"requests":1,"input_tokens":9,"output_tokens":5,"total_tokens":14,"cost":0.000068,"actual_cost":0.000136},"total":{"actual_cost":29.96545006,"cost":15.0}},"daily_usage":[{"date":"2026-10-05","requests":1,"total_tokens":14,"cost":0.000068}],"model_stats":[{"model":"gpt-6-luna","requests":1530,"total_tokens":176234829,"cost":9.03}]}`
	q := parseMoneyQuota(payload)
	if q.Kind != QuotaMoney {
		t.Fatalf("kind = %s, want money", q.Kind)
	}
	if q.Balance == nil || *q.Balance < 0.2566 || *q.Balance > 0.2568 {
		t.Errorf("balance = %v, want ~0.25667", q.Balance)
	}
	if q.BalanceUnit != "USD" {
		t.Errorf("unit = %q, want USD", q.BalanceUnit)
	}
	if q.CostTotal == nil || *q.CostTotal != 15.0 {
		t.Errorf("cost_total = %v, want 15.0", q.CostTotal)
	}
	if q.TokensToday == nil || *q.TokensToday != 14 {
		t.Errorf("tokens_today = %v, want 14", q.TokensToday)
	}
	if q.ReqsToday == nil || *q.ReqsToday != 1 {
		t.Errorf("requests_today = %v, want 1", q.ReqsToday)
	}
}

// A provider with no quota endpoint must yield ErrQuotaUnsupported, not an error
// that fails registration — and the UI then shows "measured" numbers.
func TestQuotaUnsupportedShapes(t *testing.T) {
	for _, payload := range []string{
		`{"data":[]}`,
		`{"ok":true}`,
		`not json at all`,
		`{"usage":{}}`,
	} {
		if q := parsePercentQuota(payload); q.Kind != "" {
			t.Errorf("payload %q should be unsupported, got %+v", payload, q.Kind)
		}
		if q := parseMoneyQuota(payload); q.Kind != "" {
			t.Errorf("money parse of %q should be unsupported, got %+v", payload, q.Kind)
		}
	}
}

func TestParseModelList(t *testing.T) {
	// Verbatim shape from opencode-go /v1/models (36 models; only these 4 fields).
	payload := `{"data":[{"id":"gpt-6-luna","object":"model","created":1791209756,"owned_by":"opencode"},{"id":"qwen3.8-flash","object":"model","created":1791209756,"owned_by":"opencode"}],"object":"list"}`
	var raw map[string]any
	if err := jsonUnmarshal([]byte(payload), &raw); err != nil {
		t.Fatal(err)
	}
	models := parseModelList(raw)
	if len(models) != 2 {
		t.Fatalf("parsed %d models, want 2", len(models))
	}
	if models[0].ID != "gpt-6-luna" || models[0].OwnedBy != "opencode" || models[0].Created != 1791209756 {
		t.Errorf("model[0] = %+v", models[0])
	}
	// entries without an id are skipped, not crashed on
	var raw2 map[string]any
	jsonUnmarshal([]byte(`{"data":[{"object":"model"},{"id":"ok"}]}`), &raw2)
	if got := parseModelList(raw2); len(got) != 1 || got[0].ID != "ok" {
		t.Errorf("id-less entries should be skipped: %+v", got)
	}
	// malformed
	if got := parseModelList(map[string]any{}); got != nil {
		t.Errorf("empty payload should yield nil, got %+v", got)
	}
}

func TestNewRequestIDNonEmptyAndUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := newRequestID()
		if id == "" {
			t.Fatal("empty request id — upstream would 400 MissingSessionID")
		}
		if seen[id] {
			t.Fatalf("duplicate id after %d calls", i)
		}
		seen[id] = true
	}
}
