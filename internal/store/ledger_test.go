package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := Open(ctx, Options{
		Path:      filepath.Join(dir, "test.sqlite"),
		MasterKey: "test-master-key",
		BatchSize: 4,
		BatchWait: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestOpenMigratesAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "ez.sqlite") // parent must be created
	ctx := context.Background()
	for i := 0; i < 3; i++ { // reopen twice: migration must be idempotent
		db, err := Open(ctx, Options{Path: path, MasterKey: "k"})
		if err != nil {
			t.Fatalf("open #%d: %v", i, err)
		}
		if got := db.SchemaVersion(); got != schemaVersion {
			t.Errorf("schema version = %d, want %d", got, schemaVersion)
		}
		if err := db.Close(); err != nil {
			t.Errorf("close #%d: %v", i, err)
		}
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("db file missing: %v", err)
	}
	// every expected table exists
	db, _ := Open(ctx, Options{Path: path, MasterKey: "k"})
	defer db.Close()
	want := []string{"accounts", "provider_keys", "quota_snapshots", "models", "compression_profiles",
		"combos", "combo_hops", "client_tokens", "calls", "key_cooldowns", "schema_version"}
	for _, tbl := range want {
		var n int
		if err := db.Reader().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&n); err != nil || n != 1 {
			t.Errorf("table %s missing (n=%d err=%v)", tbl, n, err)
		}
	}
	// FK order is valid: a child insert must fail without its parent
	if _, err := db.Writer().Exec(`INSERT INTO provider_keys(account_id,label,key_hint,key_plain) VALUES(999,'x','h','c')`); err == nil {
		t.Error("foreign_keys must be ON — orphan provider_keys insert should fail")
	}
}

func TestOpenRequiresMasterKey(t *testing.T) {
	if _, err := Open(context.Background(), Options{Path: filepath.Join(t.TempDir(), "x.sqlite")}); err == nil {
		t.Error("Open without MasterKey must fail")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// THE ACCOUNTING REGRESSION TEST.
//
// The three surfaces disagree about whether cached tokens are included in the
// input counter (probed live 2026-10-05):
//   openai     prompt_tokens=2044 cached=1792   -> cached INCLUDED
//   anthropic  input_tokens=252  cache_read=1792 -> cached EXCLUDED
//   responses  input_tokens=12   cached=0, cache_write=0 (+reasoning)
//
// If normalization is wrong, per-provider token totals silently disagree the
// moment traffic crosses surfaces — which is exactly what the dashboard reports.
// ─────────────────────────────────────────────────────────────────────────────

func TestNormalizeUsageSurfaceSemantics(t *testing.T) {
	cases := []struct {
		name    string
		surface Surface
		raw     string
		want    Usage
	}{
		{
			// Live probe P4: repeat of an 11.8 KB prefix on opencode-go OpenAI surface.
			name:    "openai cached-repeat (P4)",
			surface: SurfaceOpenAI,
			raw:     `{"prompt_tokens":2044,"completion_tokens":72,"total_tokens":2116,"prompt_tokens_details":{"cached_tokens":1792},"completion_tokens_details":{"reasoning_tokens":28}}`,
			want:    Usage{In: 252, Out: 72, CachedRead: 1792, CachedWrite: 0, Reasoning: 28},
		},
		{
			name:    "openai cold (P4 first call)",
			surface: SurfaceOpenAI,
			raw:     `{"prompt_tokens":2371,"completion_tokens":43,"total_tokens":2414,"prompt_tokens_details":{"cached_tokens":0},"completion_tokens_details":{"reasoning_tokens":39}}`,
			want:    Usage{In: 2371, Out: 43, CachedRead: 0, Reasoning: 39},
		},
		{
			// Live probe P5: Anthropic excludes cache_read from input_tokens.
			name:    "anthropic cached (P5)",
			surface: SurfaceAnthropic,
			raw:     `{"input_tokens":252,"output_tokens":12,"cache_read_input_tokens":1792,"cache_creation_input_tokens":0}`,
			want:    Usage{In: 252, Out: 12, CachedRead: 1792, CachedWrite: 0},
		},
		{
			// Live probe P1: tool-use call with cache creation.
			name:    "anthropic cache write",
			surface: SurfaceAnthropic,
			raw:     `{"input_tokens":315,"output_tokens":73,"cache_read_input_tokens":0,"cache_creation_input_tokens":2048}`,
			want:    Usage{In: 315, Out: 73, CachedRead: 0, CachedWrite: 2048},
		},
		{
			// Live probe P6: gpt-6-luna on /v1/responses.
			name:    "responses (P6)",
			surface: SurfaceResponses,
			raw:     `{"input_tokens":10,"output_tokens":5,"total_tokens":15,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}`,
			want:    Usage{In: 10, Out: 5},
		},
		{
			// Live probe P8: streaming response.completed carried reasoning_tokens.
			name:    "responses streamed with reasoning (P8)",
			surface: SurfaceResponses,
			raw:     `{"input_tokens":12,"output_tokens":27,"total_tokens":39,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0},"output_tokens_details":{"reasoning_tokens":13}}`,
			want:    Usage{In: 12, Out: 27, Reasoning: 13},
		},
		{
			// Responses with a real cache hit + write: cached must be subtracted
			// from input (P6 semantics), cache_write captured.
			name:    "responses cached with write",
			surface: SurfaceResponses,
			raw:     `{"input_tokens":3000,"output_tokens":40,"input_tokens_details":{"cached_tokens":2048,"cache_write_tokens":512},"output_tokens_details":{"reasoning_tokens":7}}`,
			want:    Usage{In: 952, Out: 40, CachedRead: 2048, CachedWrite: 512, Reasoning: 7},
		},
		{
			name:    "empty usage",
			surface: SurfaceOpenAI,
			raw:     `{}`,
			want:    Usage{},
		},
		{
			// Some gateways use input_tokens/output_tokens on the OpenAI surface.
			name:    "openai alternate field names",
			surface: SurfaceOpenAI,
			raw:     `{"input_tokens":500,"output_tokens":20,"prompt_tokens_details":{"cached_tokens":100}}`,
			want:    Usage{In: 400, Out: 20, CachedRead: 100},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var raw map[string]any
			if err := json.Unmarshal([]byte(tc.raw), &raw); err != nil {
				t.Fatal(err)
			}
			got := NormalizeUsage(tc.surface, raw)
			if got.In != tc.want.In || got.Out != tc.want.Out ||
				got.CachedRead != tc.want.CachedRead || got.CachedWrite != tc.want.CachedWrite ||
				got.Reasoning != tc.want.Reasoning {
				t.Errorf("got {in:%d out:%d cr:%d cw:%d rt:%d}, want {in:%d out:%d cr:%d cw:%d rt:%d}",
					got.In, got.Out, got.CachedRead, got.CachedWrite, got.Reasoning,
					tc.want.In, tc.want.Out, tc.want.CachedRead, tc.want.CachedWrite, tc.want.Reasoning)
			}
			if got.In < 0 {
				t.Errorf("tokens_in went negative: %d", got.In)
			}
		})
	}
}

// TestCrossSurfaceTotalsAgree is the property that actually matters: the SAME
// logical work reported through different surfaces must normalize to the same
// cold-input number. 2044 prompt with 1792 cached (OpenAI) == 252 input with
// 1792 cache_read (Anthropic) == 252 cold tokens either way.
func TestCrossSurfaceTotalsAgree(t *testing.T) {
	var openaiRaw, anthropicRaw map[string]any
	json.Unmarshal([]byte(`{"prompt_tokens":2044,"completion_tokens":72,"prompt_tokens_details":{"cached_tokens":1792}}`), &openaiRaw)
	json.Unmarshal([]byte(`{"input_tokens":252,"output_tokens":72,"cache_read_input_tokens":1792}`), &anthropicRaw)

	o := NormalizeUsage(SurfaceOpenAI, openaiRaw)
	a := NormalizeUsage(SurfaceAnthropic, anthropicRaw)

	if o.In != a.In {
		t.Errorf("cold input disagrees across surfaces: openai=%d anthropic=%d (must match)", o.In, a.In)
	}
	if o.CachedRead != a.CachedRead {
		t.Errorf("cached_read disagrees: openai=%d anthropic=%d", o.CachedRead, a.CachedRead)
	}
	if o.Out != a.Out {
		t.Errorf("output disagrees: openai=%d anthropic=%d", o.Out, a.Out)
	}
	// and the naive (wrong) approach would have differed:
	if openaiRaw["prompt_tokens"] == anthropicRaw["input_tokens"] {
		t.Error("test fixture is not exercising the discrepancy")
	}
}

func TestNormalizeUsageNilAndUnknown(t *testing.T) {
	if u := NormalizeUsage(SurfaceOpenAI, nil); u.In != 0 || u.Out != 0 {
		t.Errorf("nil usage must zero out, got %+v", u)
	}
	// unknown surface falls back to OpenAI-ish rather than panicking
	var raw map[string]any
	json.Unmarshal([]byte(`{"prompt_tokens":10,"completion_tokens":3}`), &raw)
	if u := NormalizeUsage(Surface("mystery"), raw); u.In != 10 || u.Out != 3 {
		t.Errorf("unknown surface fallback wrong: %+v", u)
	}
}

// cached_tokens > prompt_tokens must clamp to 0, never go negative.
func TestNormalizeUsageClampsNegative(t *testing.T) {
	var raw map[string]any
	json.Unmarshal([]byte(`{"prompt_tokens":100,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":500}}`), &raw)
	if u := NormalizeUsage(SurfaceOpenAI, raw); u.In != 0 {
		t.Errorf("tokens_in = %d, want clamped 0", u.In)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Ledger behaviour
// ─────────────────────────────────────────────────────────────────────────────

func TestRecordCallIsNonBlocking(t *testing.T) {
	db := openTest(t)
	// Flood far beyond the 512 queue without ever blocking. A blocking
	// implementation would hang this test (and a real request path).
	done := make(chan struct{})
	go func() {
		for i := 0; i < 5000; i++ {
			db.RecordCall(Call{Client: "flood", Surface: SurfaceOpenAI, Model: "m", Status: 200})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("RecordCall blocked — it must never add latency to a response")
	}
}

func TestLedgerRoundTripAndAggregation(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	now := time.Now().UTC()

	rows := []Call{
		{TS: now, Client: "claude-code", Surface: SurfaceAnthropic, Alias: "my-stack",
			Account: "super-ssn", KeyID: 1, KeyHint: "sk…def0", Model: "gpt-6.1-sol",
			Status: 200, Stream: true, TokensIn: 100, TokensOut: 50, TokensCachedRead: 20,
			RawUsage: `{"input_tokens":100}`},
		{TS: now, Client: "claude-code", Surface: SurfaceAnthropic, Alias: "my-stack",
			Account: "super-ssn", KeyID: 1, KeyHint: "sk…def0", Model: "gpt-6.1-sol",
			Status: 200, Stream: true, TokensIn: 200, TokensOut: 60, TokensCachedRead: 40},
		{TS: now, Client: "hermes", Surface: SurfaceOpenAI, Alias: "opengo/qwen3.8-flash",
			Account: "opengo", KeyID: 2, KeyHint: "sk…abcd", Model: "qwen3.8-flash",
			Status: 429, Stream: false, Err: "rate limited"},
	}
	for _, r := range rows {
		db.RecordCall(r)
	}
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}

	// group by account
	rep, err := db.UsageReport(ctx, now.Add(-time.Hour), now.Add(time.Hour), "account")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep) != 2 {
		t.Fatalf("expected 2 account groups, got %d: %+v", len(rep), rep)
	}
	byKey := map[string]UsageRow{}
	for _, r := range rep {
		byKey[r.Key] = r
	}
	ssn := byKey["super-ssn"]
	if ssn.Calls != 2 || ssn.TokensIn != 300 || ssn.TokensOut != 110 || ssn.CachedRead != 60 {
		t.Errorf("super-ssn aggregate wrong: %+v", ssn)
	}
	if ssn.Errors != 0 {
		t.Errorf("super-ssn errors = %d, want 0", ssn.Errors)
	}
	og := byKey["opengo"]
	if og.Calls != 1 || og.Errors != 1 {
		t.Errorf("opengo aggregate wrong (429 must count as error): %+v", og)
	}

	// group by client
	rep2, err := db.UsageReport(ctx, now.Add(-time.Hour), now.Add(time.Hour), "client")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep2) != 2 {
		t.Errorf("expected 2 client groups, got %d", len(rep2))
	}

	// recent calls drilldown
	recent, err := db.RecentCalls(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 3 {
		t.Fatalf("recent = %d rows, want 3", len(recent))
	}
	if recent[0].Model != "qwen3.8-flash" { // newest first
		t.Errorf("recent[0] = %s, want newest row", recent[0].Model)
	}
	if recent[0].Err != "rate limited" {
		t.Errorf("err not persisted: %q", recent[0].Err)
	}

	// bad group_by is rejected, not silently ignored
	if _, err := db.UsageReport(ctx, now, now, "nonsense"); err == nil {
		t.Error("unknown group_by must error")
	}
}

func TestLedgerRowsAreImmutableHistory(t *testing.T) {
	db := openTest(t)
	now := time.Now().UTC()

	// insert an account + key, record a call referencing them, then delete both.
	if _, err := db.Writer().Exec(`INSERT INTO accounts(id,name,namespace,kind,base_url) VALUES(1,'SSN GPT','super-ssn','openai-compatible','https://example.com/v1')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Writer().Exec(`INSERT INTO provider_keys(id,account_id,label,key_hint,key_plain) VALUES(1,1,'primary','sk…def0','sk-test-plain')`); err != nil {
		t.Fatal(err)
	}
	db.RecordCall(Call{TS: now, Client: "hermes", Surface: SurfaceOpenAI, Alias: "super-ssn/gpt-6-luna",
		Account: "super-ssn", KeyID: 1, KeyHint: "sk…def0", Model: "gpt-6-luna", Status: 200, TokensIn: 10, TokensOut: 2})
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}

	// deleting the account cascades the key — but the ledger row must survive
	// and stay readable (denormalized account/key_hint).
	if _, err := db.Writer().Exec(`DELETE FROM accounts WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	var acct, hint string
	var tin int64
	if err := db.Reader().QueryRow(`SELECT account, key_hint, tokens_in FROM calls ORDER BY id DESC LIMIT 1`).Scan(&acct, &hint, &tin); err != nil {
		t.Fatalf("ledger row lost after account deletion: %v", err)
	}
	if acct != "super-ssn" || hint != "sk…def0" || tin != 10 {
		t.Errorf("history not readable after deletion: acct=%q hint=%q in=%d", acct, hint, tin)
	}
}

func TestTTFTAndDurationsPersist(t *testing.T) {
	db := openTest(t)
	ttft, total, cms := int64(410), int64(18800), int64(3)
	db.RecordCall(Call{
		Surface: SurfaceOpenAI, Account: "a", Model: "m", Status: 200, Stream: true,
		TTFTms: &ttft, Totalms: &total, CompressionMs: &cms,
		PromptTokensPre: 1000, TokensSaved: 400, CompressionApplied: true, CompressionProfile: "agentic-safe",
	})
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}
	rows, err := db.RecentCalls(context.Background(), 1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("err=%v rows=%d", err, len(rows))
	}
	c := rows[0]
	if c.TTFTms == nil || *c.TTFTms != 410 {
		t.Errorf("ttft = %v, want 410", c.TTFTms)
	}
	if c.Totalms == nil || *c.Totalms != 18800 {
		t.Errorf("total = %v, want 18800", c.Totalms)
	}
	if !c.CompressionApplied || c.CompressionProfile != "agentic-safe" || c.PromptTokensPre != 1000 || c.TokensSaved != 400 {
		t.Errorf("compression fields wrong: %+v", c)
	}
	if !c.Stream {
		t.Error("stream flag lost")
	}
}
