package store

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// Spend counts only this client's priced calls inside the current window, at
// the price effective on each call's day — the cost report's rules.
func TestClientSpendStatusPricesTheWindow(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "s.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	acct, err := db.UpsertAccount(ctx, provider.Account{Name: "a", Namespace: "a", Kind: provider.KindOpenAICompatible, BaseURL: "http://x", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetPrice(ctx, Price{AccountID: acct, ModelID: "m", EffectiveFrom: "2020-01-01", Currency: "USD",
		PriceIn: 2, PriceOut: 10, PriceCacheRead: 1}); err != nil {
		t.Fatal(err)
	}
	id, err := db.AddClientToken(ctx, "k", "tok-k", "infer")
	if err != nil {
		t.Fatal(err)
	}
	if sp, err := db.ClientSpendStatus(ctx, "k"); err != nil || sp.Limit != 0 || sp.Spent != 0 {
		t.Fatalf("uncapped = %+v %v", sp, err)
	}
	if err := db.SetTokenCapUSD(ctx, id, 5); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	// priced: 1M in ($2) + 100k out ($1) + 1M cached read ($1) = $4
	db.RecordCall(Call{TS: now, Client: "k", AccountID: acct, Account: "a", Model: "m", Status: 200,
		TokensIn: 1_000_000, TokensOut: 100_000, TokensCachedRead: 1_000_000})
	db.RecordCall(Call{TS: now, Client: "k", AccountID: acct, Account: "a", Model: "unpriced", Status: 200, TokensIn: 9_000_000})
	db.RecordCall(Call{TS: now, Client: "other", AccountID: acct, Account: "a", Model: "m", Status: 200, TokensIn: 9_000_000})
	db.RecordCall(Call{TS: now.AddDate(0, -2, 0), Client: "k", AccountID: acct, Account: "a", Model: "m", Status: 200, TokensIn: 9_000_000})
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}
	sp, err := db.ClientSpendStatus(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	if sp.Limit != 5 || math.Abs(sp.Spent-4) > 1e-9 || sp.Window != "monthly" || sp.Bucket == "" {
		t.Fatalf("spend = %+v, want $4 of $5 this monthly", sp)
	}
	if _, err := db.ClientSpendStatus(ctx, "gone"); err != ErrNotFound {
		t.Fatalf("missing token err = %v", err)
	}
	for _, bad := range []float64{-1, math.NaN(), math.Inf(1)} {
		if err := db.SetTokenCapUSD(ctx, id, bad); err == nil {
			t.Fatalf("cap %v accepted", bad)
		}
	}
	toks, err := db.ListTokens(ctx)
	if err != nil || len(toks) != 1 || toks[0].CapUSD != 5 {
		t.Fatalf("list = %+v %v", toks, err)
	}
}
