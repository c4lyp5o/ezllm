package store

import (
	"context"
	"testing"
	"time"
)

func TestAccountingCurrencyValidation(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	id := seedPriceAccount(t, db, "currency", "currency")
	for _, currency := range []string{"EUR", "GBP", "invalid"} {
		if err := db.SetPrice(ctx, Price{AccountID: id, ModelID: "m", EffectiveFrom: "2026-01-01", Currency: currency, PriceIn: 2}); err == nil {
			t.Fatalf("accepted %q as USD", currency)
		}
	}
	for _, currency := range []string{"", "USD", " usd "} {
		if err := db.SetPrice(ctx, Price{AccountID: id, ModelID: "m", EffectiveFrom: "2026-01-01", Currency: currency, PriceIn: 2}); err != nil {
			t.Fatal(err)
		}
	}
	prices, err := db.ListPrices(ctx)
	if err != nil || len(prices) != 1 || prices[0].Currency != "USD" {
		t.Fatalf("prices=%+v err=%v", prices, err)
	}
}

func TestAccountingLegacyCurrencyIsUnpriced(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	id := seedPriceAccount(t, db, "legacy-currency", "legacy-currency")
	mustPrice(t, db, id, "m", "2026-01-01", 1, 1, 1, 1)
	mustPrice(t, db, id, "m", "2026-10-01", 2, 2, 2, 2)
	if _, err := db.w.Exec(`UPDATE model_prices SET currency='EUR' WHERE effective_from='2026-10-01'`); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	if err := db.insertCalls(ctx, []Call{{TS: now, AccountID: id, Model: "m", TokensIn: 1_000_000}}); err != nil {
		t.Fatal(err)
	}
	rows, err := db.CostReport(ctx, now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].USD != 0 || rows[0].UnpricedCalls != 1 {
		t.Fatalf("non-USD rate mislabeled or older USD used: %+v", rows)
	}
}
