package store

import (
	"context"
	"testing"
	"time"
)

// seedAccountForRule creates a minimal account so a model_rule can FK to it.
func seedAccountForRule(t *testing.T, db *DB, namespace string) int64 {
	t.Helper()
	ctx := context.Background()
	id, err := db.CreateAccount(ctx, AccountInput{
		Name:      namespace + " display",
		Namespace: namespace,
		Kind:      "openai-compatible",
		BaseURL:   "https://example.invalid/v1",
	})
	if err != nil {
		t.Fatalf("CreateAccount(%s): %v", namespace, err)
	}
	return id
}

// TestModelRuleValidation pins the invariant set — a malformed window must be
// rejected at write time, never discovered by the router at request time.
func TestModelRuleValidation(t *testing.T) {
	cases := []struct {
		name    string
		rule    ModelRule
		wantErr bool
	}{
		{"ok no window", ModelRule{AccountID: 1, ModelID: "m", CapWindow: "monthly"}, false},
		{"ok window", ModelRule{AccountID: 1, ModelID: "m", CapWindow: "daily",
			WinStart: "22:00", WinEnd: "06:00", WinTZ: "Asia/Kuala_Lumpur"}, false},
		{"ok every day empty win_days", ModelRule{AccountID: 1, ModelID: "m",
			WinStart: "09:00", WinEnd: "17:00"}, false},
		{"reject missing model", ModelRule{AccountID: 1, ModelID: "  "}, true},
		{"reject negative cap", ModelRule{AccountID: 1, ModelID: "m", CapTokens: -1}, true},
		{"reject bad window enum", ModelRule{AccountID: 1, ModelID: "m", CapWindow: "hourly"}, true},
		{"reject bad start", ModelRule{AccountID: 1, ModelID: "m", WinStart: "9:00", WinEnd: "17:00"}, true},
		{"reject hour 24", ModelRule{AccountID: 1, ModelID: "m", WinStart: "24:00", WinEnd: "25:00"}, true},
		{"reject bad end", ModelRule{AccountID: 1, ModelID: "m", WinStart: "09:00", WinEnd: "noon"}, true},
		{"reject identical window", ModelRule{AccountID: 1, ModelID: "m",
			WinStart: "09:00", WinEnd: "09:00"}, true},
		{"reject half window", ModelRule{AccountID: 1, ModelID: "m", WinStart: "09:00"}, true},
		{"reject bad day", ModelRule{AccountID: 1, ModelID: "m", WinDays: "7"}, true},
		{"reject negative day", ModelRule{AccountID: 1, ModelID: "m", WinDays: "-1"}, true},
		{"reject non-numeric day", ModelRule{AccountID: 1, ModelID: "m", WinDays: "Mon"}, true},
		{"reject duplicate day", ModelRule{AccountID: 1, ModelID: "m", WinDays: "1,1"}, true},
		{"reject bad tz", ModelRule{AccountID: 1, ModelID: "m", WinTZ: "Mars/Olympus"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateModelRule(&c.rule)
			if c.wantErr && err == nil {
				t.Error("want error, got nil")
			}
			if !c.wantErr && err != nil {
				t.Errorf("want ok, got %v", err)
			}
		})
	}
}

// TestModelRuleCRUDRoundTrip: upsert inserts, a second upsert on the same
// (account, model) updates in place rather than duplicating (the UNIQUE key is
// the router's lookup key), and delete removes the restriction.
func TestModelRuleCRUDRoundTrip(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	aid := seedAccountForRule(t, db, "crud")

	// Insert.
	id1, err := db.UpsertModelRule(ctx, ModelRule{
		AccountID: aid, ModelID: "nightly", CapTokens: 5_000_000, CapWindow: "monthly",
		WinStart: "22:00", WinEnd: "06:00", WinDays: "1,2,3,4,5",
		WinTZ: "Asia/Kuala_Lumpur", Enabled: true, Note: "night discount",
	})
	if err != nil {
		t.Fatalf("upsert insert: %v", err)
	}

	// Update in place: same key, new values → same row id, one row total.
	id2, err := db.UpsertModelRule(ctx, ModelRule{
		AccountID: aid, ModelID: "nightly", CapTokens: 9_999, CapWindow: "daily",
		WinTZ: "Asia/Kuala_Lumpur", Enabled: false,
	})
	if err != nil {
		t.Fatalf("upsert update: %v", err)
	}
	if id1 != id2 {
		t.Errorf("row id changed on update: %d -> %d (UNIQUE key must be stable)", id1, id2)
	}

	got, err := db.GetModelRule(ctx, id1)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.CapTokens != 9_999 || got.CapWindow != "daily" {
		t.Errorf("update not applied: cap=%d window=%q", got.CapTokens, got.CapWindow)
	}
	if got.Enabled {
		t.Error("enabled update not applied (want false)")
	}

	// List sees exactly one rule for this account.
	list, err := db.ListModelRules(ctx, aid)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("rules for account = %d, want 1", len(list))
	}

	// Delete removes the restriction.
	if err := db.DeleteModelRule(ctx, id1); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := db.GetModelRule(ctx, id1); err != ErrRuleNotFound {
		t.Errorf("after delete, get = %v, want ErrRuleNotFound", err)
	}
	if err := db.DeleteModelRule(ctx, id1); err != ErrRuleNotFound {
		t.Errorf("second delete = %v, want ErrRuleNotFound", err)
	}
}

// TestUsageCounterMetering drives the real ledger flush and asserts the meter
// advanced in the same transaction as the calls — the property that keeps the
// meter and the ledger from ever disagreeing about what counted.
func TestUsageCounterMetering(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	aid := seedAccountForRule(t, db, "meter")

	// A rule must exist or the model is not metered (nothing caps it).
	if _, err := db.UpsertModelRule(ctx, ModelRule{
		AccountID: aid, ModelID: "m1", CapWindow: "daily",
		WinTZ: "Asia/Kuala_Lumpur", Enabled: true,
	}); err != nil {
		t.Fatalf("rule: %v", err)
	}

	kl, _ := time.LoadLocation("Asia/Kuala_Lumpur")
	now := time.Now().In(kl)
	wantBucket := CountBucket(now, "daily", kl)

	// Three calls: 100 + 200 tokens_in, 50 each out = 400 counted tokens.
	rows := []Call{
		{TS: now, AccountID: aid, Model: "m1", TokensIn: 100, TokensOut: 50},
		{TS: now, AccountID: aid, Model: "m1", TokensIn: 200, TokensOut: 50},
		{TS: now, AccountID: aid, Model: "m2", TokensIn: 999, TokensOut: 1}, // no rule → not metered
	}
	if err := db.insertCalls(ctx, rows); err != nil {
		t.Fatalf("insertCalls: %v", err)
	}

	c, err := db.UsageFor(ctx, aid, "m1", wantBucket)
	if err != nil {
		t.Fatalf("UsageFor: %v", err)
	}
	if c.Tokens != 400 {
		t.Errorf("metered tokens = %d, want 400 (100+50 + 200+50; cached/reasoning excluded)", c.Tokens)
	}
	if c.Calls != 2 {
		t.Errorf("metered calls = %d, want 2", c.Calls)
	}

	// m2 has no rule → no bucket row must exist for it.
	other, err := db.UsageFor(ctx, aid, "m2", wantBucket)
	if err != nil {
		t.Fatalf("UsageFor m2: %v", err)
	}
	if other.Tokens != 0 || other.Calls != 0 {
		t.Errorf("model without a rule was metered: %+v (want zero — nothing caps it)", other)
	}
}

// TestUsageForMissingIsZero: an uncalled model is not over its cap. Gating a
// cap on "the row exists" would refuse every model's first request.
func TestUsageForMissingIsZero(t *testing.T) {
	db := openTest(t)
	c, err := db.UsageFor(context.Background(), 999, "never", "2099-01")
	if err != nil {
		t.Fatalf("want zero-value not error, got %v", err)
	}
	if c.Tokens != 0 || c.Calls != 0 {
		t.Errorf("got %+v, want zero usage", c)
	}
}
