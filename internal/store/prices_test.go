package store

// v8 (M9) pricing math. The traps this file pins:
//   - cost is derived from the RATE EFFECTIVE ON THE CALL'S DATE, not the
//     newest rate overall (a reprice must not rewrite history);
//   - unpriced calls are counted, never silently zeroed into the total;
//   - the four token columns partition input with no overlap (tokens_in
//     already excludes cached), so the weighted sum is the whole bill.

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"
)

func mustPrice(t *testing.T, db *DB, acct int64, model, day string, in, out, cr, cw float64) {
	t.Helper()
	if err := db.SetPrice(context.Background(), Price{
		AccountID: acct, ModelID: model, EffectiveFrom: day,
		PriceIn: in, PriceOut: out, PriceCacheRead: cr, PriceCacheWrite: cw,
	}); err != nil {
		t.Fatalf("SetPrice: %v", err)
	}
}

// seedPriceAccount creates one account and returns its id — model_prices has a
// real FK to accounts, so a rate can never float free of an account.
func seedPriceAccount(t *testing.T, db *DB, name, ns string) int64 {
	t.Helper()
	id, err := db.CreateAccount(context.Background(), AccountInput{
		Name: name, Namespace: ns, Kind: "openai-compatible",
		BaseURL: "https://example.invalid/v1", Enabled: ptrTrue(),
	})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	return id
}

func seedCall(t *testing.T, db *DB, ts time.Time, acctID int64, model string, in, out, cr, cw int64) {
	t.Helper()
	db.RecordCall(Call{TS: ts, Surface: SurfaceOpenAI, AccountID: acctID, Account: "acct", Model: model,
		Status: 200, TokensIn: in, TokensOut: out, TokensCachedRead: cr, TokensCachedWrite: cw})
}

func TestCostReportDerivesFromEffectiveRates(t *testing.T) {
	db := openAt(t, filepath.Join(t.TempDir(), "cost.sqlite"))
	defer db.Close()
	ctx := context.Background()
	acct := seedPriceAccount(t, db, "cost-a", "costa")

	// Rate history: cheap until 2026-06-01, then double.
	mustPrice(t, db, acct, "m", "2026-01-01", 1.0, 2.0, 0.1, 1.25)
	mustPrice(t, db, acct, "m", "2026-06-01", 2.0, 4.0, 0.2, 2.5)

	jan := time.Date(2026, 2, 10, 12, 0, 0, 0, time.UTC)
	jul := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	// Each call: in=1M full-price, out=1M, read=1M, write=1M.
	seedCall(t, db, jan, acct, "m", 1_000_000, 1_000_000, 1_000_000, 1_000_000)
	seedCall(t, db, jul, acct, "m", 1_000_000, 1_000_000, 1_000_000, 1_000_000)
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}

	rows, err := db.CostReport(ctx, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (one per day)", len(rows))
	}
	byDay := map[string]CostRow{}
	for _, r := range rows {
		byDay[r.Day] = r
	}
	// Feb rides the Jan rate: 1*1 + 2*1 + 0.1*1 + 1.25*1 = 4.35 USD
	if got := byDay["2026-02-10"].USD; math.Abs(got-4.35) > 1e-9 {
		t.Errorf("Feb usd = %v, want 4.35 (must use the rate EFFECTIVE then)", got)
	}
	// Jul rides the Jun rate: 2 + 4 + 0.2 + 2.5 = 8.70
	if got := byDay["2026-07-10"].USD; math.Abs(got-8.70) > 1e-9 {
		t.Errorf("Jul usd = %v, want 8.70", got)
	}
}

func TestCostReportSeparatesUnpriced(t *testing.T) {
	db := openAt(t, filepath.Join(t.TempDir(), "cost2.sqlite"))
	defer db.Close()
	ctx := context.Background()
	acct := seedPriceAccount(t, db, "cost-b", "costb")

	mustPrice(t, db, acct, "priced", "2026-01-01", 1, 1, 0, 0)
	now := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	seedCall(t, db, now, acct, "priced", 1_000_000, 0, 0, 0) // = 1.00 USD
	seedCall(t, db, now, acct, "ghost", 99_000_000, 0, 0, 0) // no rate at all
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}

	rows, err := db.CostReport(ctx, now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var total float64
	var unpriced int64
	ghostSeen := false
	for _, r := range rows {
		total += r.USD
		unpriced += r.UnpricedCalls
		if r.Model == "ghost" {
			ghostSeen = true
			if r.UnpricedCalls != 1 || r.USD != 0 {
				t.Errorf("ghost row: unpriced=%d usd=%v, want 1/0", r.UnpricedCalls, r.USD)
			}
		}
	}
	if !ghostSeen {
		t.Fatal("ghost model missing from the report entirely")
	}
	// The huge unpriced prompt must NOT leak money into the total — and must
	// be VISIBLE as unpriced so nobody reads 1.00 as the full bill.
	if math.Abs(total-1.0) > 1e-9 {
		t.Errorf("total usd = %v, want exactly 1.00 (unpriced excluded)", total)
	}
	if unpriced != 1 {
		t.Errorf("unpriced_calls = %d, want 1", unpriced)
	}
}

func TestPriceValidation(t *testing.T) {
	db := openAt(t, filepath.Join(t.TempDir(), "val.sqlite"))
	defer db.Close()
	ctx := context.Background()
	acct := seedPriceAccount(t, db, "val-a", "vala")

	if err := db.SetPrice(ctx, Price{AccountID: 0, ModelID: "m", EffectiveFrom: "2026-01-01"}); err == nil {
		t.Error("account_id 0 accepted")
	}
	if err := db.SetPrice(ctx, Price{AccountID: acct, ModelID: "", EffectiveFrom: "2026-01-01"}); err == nil {
		t.Error("empty model accepted")
	}
	if err := db.SetPrice(ctx, Price{AccountID: acct, ModelID: "m", EffectiveFrom: "last tuesday"}); err == nil {
		t.Error("garbage effective_from accepted")
	}
	if err := db.SetPrice(ctx, Price{AccountID: acct, ModelID: "m", EffectiveFrom: "2026-01-01", PriceIn: -1}); err == nil {
		t.Error("negative rate accepted")
	}
	// RFC3339 timestamps normalize to the date prefix instead of being refused.
	if err := db.SetPrice(ctx, Price{AccountID: acct, ModelID: "m", EffectiveFrom: "2026-01-01T00:00:00Z",
		PriceIn: 1, PriceOut: 1}); err != nil {
		t.Fatalf("timestamp effective_from refused: %v", err)
	}
	list, err := db.ListPrices(ctx)
	if err != nil || len(list) != 1 || list[0].EffectiveFrom != "2026-01-01" || list[0].Currency != "USD" {
		t.Fatalf("list after normalize = %+v err=%v", list, err)
	}
	// Re-set the same version overwrites (upsert), it does not duplicate.
	mustPrice(t, db, acct, "m", "2026-01-01", 9, 9, 0, 0)
	if list, _ := db.ListPrices(ctx); len(list) != 1 || list[0].PriceIn != 9 {
		t.Fatalf("upsert misbehaved: %+v", list)
	}
	// Delete is exact-version: wrong date deletes nothing.
	if err := db.DeletePrice(ctx, acct, "m", "2025-01-01"); err == nil {
		t.Error("delete with wrong effective_from claimed success")
	}
	if err := db.DeletePrice(ctx, acct, "m", "2026-01-01"); err != nil {
		t.Errorf("delete exact version: %v", err)
	}
	if list, _ := db.ListPrices(ctx); len(list) != 0 {
		t.Fatalf("rows survived delete: %+v", list)
	}
}
