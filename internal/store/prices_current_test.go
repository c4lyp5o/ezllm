package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestCurrentPricesSelectsEffectiveVersionAndProvenance(t *testing.T) {
	db := openAt(t, filepath.Join(t.TempDir(), "current.sqlite"))
	defer db.Close()
	ctx := context.Background()
	acct := seedPriceAccount(t, db, "price-current", "pc")

	if err := db.SetPrice(ctx, Price{
		AccountID: acct, ModelID: "m", EffectiveFrom: "2026-01-01",
		PriceIn: 1, PriceOut: 2, PriceCacheRead: 0.1, PriceCacheWrite: 0.2,
		Source: "manual",
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetPrice(ctx, Price{
		AccountID: acct, ModelID: "m", EffectiveFrom: "2026-06-01",
		PriceIn: 3, PriceOut: 4, PriceCacheRead: 0.3, PriceCacheWrite: 0.4,
		Source: "litellm", FetchedAt: "2026-06-01T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}

	jan, err := db.CurrentPrices(ctx, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), acct, "m")
	if err != nil || len(jan) != 1 || jan[0].PriceIn != 1 || jan[0].EffectiveFrom != "2026-01-01" {
		t.Fatalf("january = %+v err=%v, want the January row", jan, err)
	}
	jul, err := db.CurrentPrices(ctx, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), acct, "m")
	if err != nil || len(jul) != 1 || jul[0].PriceIn != 3 || jul[0].Source != "litellm" {
		t.Fatalf("july = %+v err=%v, want the June row with source", jul, err)
	}
	all, err := db.CurrentPrices(ctx, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), 0, "")
	if err != nil || len(all) != 1 {
		t.Fatalf("unfiltered current = %+v err=%v", all, err)
	}
}
