package store

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

type rollCall struct {
	age            time.Duration
	acct           int64
	ns, model, cli string
	key            int64
	status         int
	in, out, cr    int64
	applied        bool
}

func rollCalls(t *testing.T, db *DB, now time.Time, calls []rollCall) []int64 {
	t.Helper()
	var ids []int64
	for _, c := range calls {
		res, err := db.Writer().Exec(`INSERT INTO calls (ts, account_id, client, surface, alias, account, provider_key_id, key_hint, model, status,
		  tokens_in, tokens_out, tokens_cached_read, tokens_saved, compression_applied, context_tokens_pre, context_tokens_saved)
		  VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			now.Add(-c.age).UTC().Format("2006-01-02T15:04:05.000Z"), c.acct, c.cli, "chat", "daily", c.ns, c.key, "hint-"+c.ns, c.model, c.status,
			c.in, c.out, c.cr, 7, boolToInt(c.applied), 100, 9)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		ids = append(ids, id)
	}
	return ids
}

// rollReports reads everything a long-range chart or cost panel shows, minus the
// median TTFT (which needs raw calls).
func rollReports(t *testing.T, db *DB, from, to time.Time) map[string]any {
	t.Helper()
	ctx := context.Background()
	out := map[string]any{}
	for _, g := range []string{"account", "key", "alias", "client", "model", "surface", "day"} {
		rows, err := db.UsageReport(ctx, from, to, g)
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].P50TTFTms = 0
		}
		out["usage:"+g] = rows
	}
	days, err := db.UsageDayReportTZ(ctx, from, to, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	for i := range days {
		days[i].P50TTFTms = 0
	}
	out["days"] = days
	cost, err := db.CostReport(ctx, from, to)
	if err != nil {
		t.Fatal(err)
	}
	out["cost"] = cost
	return out
}

// Rolling up old calls deletes them and leaves every report exactly as it was.
func TestRollUpLedgerKeepsReportsWhole(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	a, err := db.UpsertAccount(ctx, provider.Account{Name: "acme", Namespace: "acme", Kind: provider.KindOpenAICompatible, BaseURL: "https://api.example.com/v1", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := db.UpsertAccount(ctx, provider.Account{Name: "globex", Namespace: "globex", Kind: provider.KindOpenAICompatible, BaseURL: "https://g.example.com/v1", Enabled: true})
	if _, err := db.Writer().Exec(`INSERT INTO model_prices (account_id, model_id, effective_from, price_in, price_out, price_cache_read) VALUES (?, 'm1', '2000-01-01', 1, 2, 0.5)`, a); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	day := 24 * time.Hour
	rollCalls(t, db, now, []rollCall{
		{40*day + time.Hour, a, "acme", "m1", "app", 1, 200, 1000, 200, 50, true},
		{40*day + 2*time.Hour, a, "acme", "m1", "app", 1, 200, 3000, 100, 0, false},
		{40*day + 3*time.Hour, a, "acme", "m1", "bot", 2, 429, 0, 0, 0, false},
		{41 * day, b, "globex", "m2", "app", 3, 200, 500, 500, 0, false},
		{50 * day, a, "acme", "m1", "", 1, 500, 10, 0, 0, false},
		{2 * day, a, "acme", "m1", "app", 1, 200, 70, 30, 0, false},
	})
	from, to := now.Add(-365*day), now.Add(time.Hour)
	before := rollReports(t, db, from, to)

	days, rolled, err := db.RollUpLedger(ctx, now.AddDate(0, 0, -MinRetentionDays))
	if err != nil || rolled != 5 {
		t.Fatalf("rolled %d calls over %d days (%v), want 5", rolled, days, err)
	}
	var left int
	_ = db.Reader().QueryRow(`SELECT COUNT(*) FROM calls`).Scan(&left)
	if left != 1 {
		t.Fatalf("calls left = %d, want the recent one", left)
	}
	after := rollReports(t, db, from, to)
	for k := range before {
		if !reflect.DeepEqual(before[k], after[k]) {
			t.Errorf("%s changed:\nbefore %+v\nafter  %+v", k, before[k], after[k])
		}
	}
	// Nothing left to roll: a second run is a no-op.
	if days, rolled, err := db.RollUpLedger(ctx, now.AddDate(0, 0, -MinRetentionDays)); err != nil || days != 0 || rolled != 0 {
		t.Fatalf("second run: %d days %d calls %v", days, rolled, err)
	}
}

// A late call on an already rolled day adds to that day's totals.
func TestRollUpLedgerMergesIntoARolledDay(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	a, _ := db.UpsertAccount(ctx, provider.Account{Name: "acme", Namespace: "acme", Kind: provider.KindOpenAICompatible, BaseURL: "https://api.example.com/v1", Enabled: true})
	now := time.Date(2026, 11, 26, 18, 0, 0, 0, time.UTC)
	old := 40 * 24 * time.Hour
	rollCalls(t, db, now, []rollCall{{old, a, "acme", "m1", "app", 1, 200, 10, 1, 0, false}})
	if _, _, err := db.RollUpLedger(ctx, now.AddDate(0, 0, -MinRetentionDays)); err != nil {
		t.Fatal(err)
	}
	rollCalls(t, db, now, []rollCall{{old + time.Minute, a, "acme", "m1", "app", 1, 200, 5, 1, 0, false}})
	if _, _, err := db.RollUpLedger(ctx, now.AddDate(0, 0, -MinRetentionDays)); err != nil {
		t.Fatal(err)
	}
	var rows, calls, tin int64
	if err := db.Reader().QueryRow(`SELECT COUNT(*), SUM(calls), SUM(tokens_in) FROM calls_daily`).Scan(&rows, &calls, &tin); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || calls != 2 || tin != 15 {
		t.Fatalf("calls_daily rows=%d calls=%d tokens_in=%d, want one row of 2 calls and 15 tokens", rows, calls, tin)
	}
}

// A call whose meter debit is still pending stays until it is reconciled,
// and only whole UTC days before the cutoff are rolled.
func TestRollUpLedgerKeepsPendingAndPartialDays(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	a, _ := db.UpsertAccount(ctx, provider.Account{Name: "acme", Namespace: "acme", Kind: provider.KindOpenAICompatible, BaseURL: "https://api.example.com/v1", Enabled: true})
	now := time.Date(2026, 11, 26, 18, 0, 0, 0, time.UTC)
	cutoff := now.AddDate(0, 0, -MinRetentionDays) // 2026-10-25 18:00: days before 2026-10-25 roll
	ids := rollCalls(t, db, now, []rollCall{
		{40 * 24 * time.Hour, a, "acme", "m1", "app", 1, 200, 10, 1, 0, false},
		{40*24*time.Hour + time.Minute, a, "acme", "m1", "app", 1, 200, 10, 1, 0, false},
	})
	if _, err := db.Writer().Exec(`INSERT INTO pending_usage_meter (call_id) VALUES (?)`, ids[1]); err != nil {
		t.Fatal(err)
	}
	// A call on the cutoff's own day, six hours before the cutoff time.
	rollCalls(t, db, now, []rollCall{{now.Sub(cutoff) + 6*time.Hour, a, "acme", "m1", "app", 1, 200, 1, 1, 0, false}})
	_, rolled, err := db.RollUpLedger(ctx, cutoff)
	if err != nil || rolled != 1 {
		t.Fatalf("rolled %d (%v), want only the settled old call", rolled, err)
	}
	var left int
	_ = db.Reader().QueryRow(`SELECT COUNT(*) FROM calls`).Scan(&left)
	if left != 2 {
		t.Fatalf("calls left = %d, want the pending one and the cutoff day's", left)
	}
}

// A key whose calls were all rolled up still shows when it was last used.
func TestTokenLastUsedSurvivesRollUp(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	a, _ := db.UpsertAccount(ctx, provider.Account{Name: "acme", Namespace: "acme", Kind: provider.KindOpenAICompatible, BaseURL: "https://api.example.com/v1", Enabled: true})
	if _, err := db.AddClientToken(ctx, "my-app", "ezllm-test-token-0000000000000000", "infer"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 26, 18, 0, 0, 0, time.UTC)
	rollCalls(t, db, now, []rollCall{{40 * 24 * time.Hour, a, "acme", "m1", "my-app", 1, 200, 1, 1, 0, false}})
	if _, _, err := db.RollUpLedger(ctx, now.AddDate(0, 0, -MinRetentionDays)); err != nil {
		t.Fatal(err)
	}
	tokens, err := db.ListTokens(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range tokens {
		if tok.Name == "my-app" {
			if tok.LastUsedAt == nil || *tok.LastUsedAt != "2026-10-17T12:00:00.000Z" {
				t.Fatalf("last used = %v, want the rolled day", tok.LastUsedAt)
			}
			return
		}
	}
	t.Fatal("token not listed")
}
