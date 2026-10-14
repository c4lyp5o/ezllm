package store

import (
	"context"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// InsertQuotaSnapshot → read-back for the TODAY COUNTERS. The columns
// (tokens_today, requests_today) existed in the schema before this test and
// were silently dropped by the INSERT; this is the round-trip that proves the
// wiring (insert → LatestQuotas → hydrateAccount/ListAccounts).
func TestQuotaSnapshotCountersRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := openEventsDB(t, 4)
	defer db.Close()

	accID, err := db.CreateAccount(ctx, AccountInput{
		Name: "Counting Provider", Namespace: "counting", Kind: "openai-compatible",
		BaseURL: "https://counting.example/v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	keyID, _, err := db.AddKey(ctx, accID, "primary", "sk-counting-key")
	if err != nil {
		t.Fatal(err)
	}

	// A count-shaped read (Ollama class): tokens_today absent (nil ⇒ NULL),
	// requests_today a REAL ZERO that must survive the round trip as 0, not NULL.
	zero := int64(0)
	nine := int64(9)
	if err := db.InsertQuotaSnapshot(ctx, keyID, provider.Quota{
		Kind: provider.QuotaTokens, Raw: `{"range":"24h","totals":{"request_count":0}}`,
		ReqsToday: &zero,
	}, true, ""); err != nil {
		t.Fatal(err)
	}
	// Second read with real tokens + requests — must win as the LATEST.
	if err := db.InsertQuotaSnapshot(ctx, keyID, provider.Quota{
		Kind: provider.QuotaMoney, Raw: `{"balance":"1.00"}`,
		TokensToday: &nine, ReqsToday: &nine,
	}, true, ""); err != nil {
		t.Fatal(err)
	}

	// --- LatestQuotas projection (/admin/quota + export) ---
	rows, err := db.LatestQuotas(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("LatestQuotas rows = %d, want 1", len(rows))
	}
	q := rows[0]
	if q["kind"] != "money" {
		t.Errorf("latest kind = %v, want money (newest read wins)", q["kind"])
	}
	if tok, ok := q["tokens_today"].(int64); !ok || tok != 9 {
		t.Errorf("tokens_today = %#v, want int64(9)", q["tokens_today"])
	}
	if req, ok := q["requests_today"].(int64); !ok || req != 9 {
		t.Errorf("requests_today = %#v, want int64(9)", q["requests_today"])
	}

	// --- hydrateAccount projection (providers page card) ---
	accs, err := db.ListAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(accs) != 1 || accs[0].Quota == nil {
		t.Fatalf("account quota missing: %#v", accs)
	}
	if accs[0].Quota.TokensToday == nil || *accs[0].Quota.TokensToday != 9 {
		t.Errorf("summary tokens_today = %v, want 9", accs[0].Quota.TokensToday)
	}
	if accs[0].Quota.RequestsToday == nil || *accs[0].Quota.RequestsToday != 9 {
		t.Errorf("summary requests_today = %v, want 9", accs[0].Quota.RequestsToday)
	}

	// A provider whose read carries NO counters must READ BACK AS ABSENT
	// (NULL), not as a fabricated zero.
	if err := db.InsertQuotaSnapshot(ctx, keyID, provider.Quota{
		Kind: provider.QuotaMoney, Raw: `{"balance":"0.00"}`,
	}, true, ""); err != nil {
		t.Fatal(err)
	}
	accs, err = db.ListAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	acc := accs[0]
	if acc.Quota == nil {
		t.Fatal("quota vanished")
	}
	if acc.Quota.TokensToday != nil {
		t.Errorf("tokens_today = %v, want nil when provider reported none", acc.Quota.TokensToday)
	}
	if acc.Quota.RequestsToday != nil {
		t.Errorf("requests_today = %v, want nil when provider reported none", acc.Quota.RequestsToday)
	}
}
