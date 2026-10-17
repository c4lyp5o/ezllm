package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestBucketRangeFiveHourLastBlockEndsAtMidnight(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Kuala_Lumpur")
	if err != nil {
		t.Fatal(err)
	}
	start, end := BucketRange(time.Date(2026, 10, 18, 22, 30, 0, 0, loc), "5h", loc)
	if start.Hour() != 20 || end.Hour() != 0 || end.Day() != 19 {
		t.Fatalf("range = %s .. %s, want 20:00 .. next midnight", start, end)
	}
}

func TestClientBudgetRejectsNegativeCap(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "b.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	id, err := db.AddClientToken(ctx, "neg-key", "tok-neg", "infer")
	if err != nil {
		t.Fatal(err)
	}
	bad := int64(-1)
	if err := db.UpdateToken(ctx, id, nil, nil, &bad, "daily", nil, nil); err == nil {
		t.Fatal("negative cap_tokens must be rejected")
	}
}

func TestClientBudgetMeterAndStatus(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "b.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	id, err := db.AddClientToken(ctx, "budget-key", "tok-budget", "infer")
	if err != nil {
		t.Fatal(err)
	}
	limit := int64(100)
	if err := db.UpdateToken(ctx, id, nil, nil, &limit, "daily", nil, nil); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	if err := db.insertCalls(ctx, []Call{{
		TS: now, Client: "budget-key", AccountID: 1, Model: "m",
		TokensIn: 60, TokensOut: 40, Status: 200,
	}}); err != nil {
		t.Fatal(err)
	}

	b, err := db.ClientBudgetStatus(ctx, "budget-key")
	if err != nil {
		t.Fatal(err)
	}
	if b.Limit != 100 || b.Used != 100 || b.Window != "daily" {
		t.Fatalf("budget = %+v, want limit=100 used=100 window=daily", b)
	}

	// One more token is over the cap.
	if err := db.insertCalls(ctx, []Call{{
		TS: now, Client: "budget-key", AccountID: 1, Model: "m",
		TokensIn: 1, Status: 200,
	}}); err != nil {
		t.Fatal(err)
	}
	b, err = db.ClientBudgetStatus(ctx, "budget-key")
	if err != nil {
		t.Fatal(err)
	}
	if b.Used != 101 {
		t.Fatalf("used = %d, want 101 after the second call", b.Used)
	}
}

func TestClientBudgetBackfillsCapSetAfterUsage(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "b.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	id, err := db.AddClientToken(ctx, "late-key", "tok-late", "infer")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.insertCalls(ctx, []Call{{
		TS: time.Now().UTC(), Client: "late-key", AccountID: 1, Model: "m",
		TokensIn: 70, TokensOut: 30, Status: 200,
	}}); err != nil {
		t.Fatal(err)
	}
	// Simulate a key that spent before any meter existed (old database): drop
	// the counter, then set a cap. The first status check must backfill it.
	if _, err := db.w.ExecContext(ctx, `DELETE FROM client_usage_counters WHERE client = 'late-key'`); err != nil {
		t.Fatal(err)
	}
	// The cap is set after the spend: the counter row does not exist yet.
	limit := int64(100)
	if err := db.UpdateToken(ctx, id, nil, nil, &limit, "daily", nil, nil); err != nil {
		t.Fatal(err)
	}
	b, err := db.ClientBudgetStatus(ctx, "late-key")
	if err != nil {
		t.Fatal(err)
	}
	if b.Used != 100 {
		t.Fatalf("backfilled used = %d, want 100", b.Used)
	}
}

func TestClientBudgetUnlimitedTokenHasNoCounter(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "b.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := db.UpsertClientToken(ctx, "free-key", "tok-free", "infer"); err != nil {
		t.Fatal(err)
	}
	if err := db.insertCalls(ctx, []Call{{
		TS: time.Now().UTC(), Client: "free-key", AccountID: 1, Model: "m",
		TokensIn: 10, TokensOut: 5, Status: 200,
	}}); err != nil {
		t.Fatal(err)
	}
	b, err := db.ClientBudgetStatus(ctx, "free-key")
	if err != nil {
		t.Fatal(err)
	}
	if b.Limit != 0 || b.Used != 0 {
		t.Fatalf("unlimited budget = %+v, want limit=0 used=0", b)
	}
}
