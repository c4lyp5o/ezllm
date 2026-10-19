package store

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// The meter is best-effort but all-or-nothing: when one part of it fails (here
// the client counters), the model counters bumped earlier in the same batch
// roll back too, and the calls rows still commit.
func TestMeterFailureRollsBackTheWholeBatchMeter(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	aid := seedAccountForRule(t, db, "atomic")
	if _, err := db.UpsertModelRule(ctx, ModelRule{AccountID: aid, ModelID: "m1", CapWindow: "daily", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddClientToken(ctx, "c1", "tok-c1", "infer"); err != nil {
		t.Fatal(err)
	}
	// Break only the client half of the meter.
	if _, err := db.w.Exec(`DROP TABLE client_usage_counters`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	before := UnmeteredRows()
	if err := db.insertCalls(ctx, []Call{
		{TS: now, AccountID: aid, Client: "c1", Model: "m1", TokensIn: 100, TokensOut: 50},
		{TS: now, AccountID: aid, Client: "c1", Model: "m1", TokensIn: 1, TokensOut: 1},
	}); err != nil {
		t.Fatalf("a meter failure must not fail the insert: %v", err)
	}
	if got := UnmeteredRows() - before; got != 2 {
		t.Errorf("unmetered_rows delta = %d, want 2 — an unmetered batch must be visible, not one Warn line", got)
	}
	var calls int
	if err := db.r.QueryRow(`SELECT COUNT(*) FROM calls`).Scan(&calls); err != nil || calls != 2 {
		t.Fatalf("calls rows = %d (err %v), want 2", calls, err)
	}
	c, err := db.UsageFor(ctx, aid, "m1", CountBucket(now, "daily", time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if c.Tokens != 0 || c.Calls != 0 {
		t.Errorf("model counter = %+v after a failed meter, want zero (rolled back with the client half)", c)
	}
}

// The tz-aware day report is the Go form of the SQL day report: for UTC both
// must agree on every field, applied_saved included.
func TestUsageDayReportTZMatchesSQLDayReport(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	day := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	ms := func(v int64) *int64 { return &v }
	if err := db.insertCalls(ctx, []Call{
		{TS: day, Model: "a", Status: 200, TokensIn: 10, TokensOut: 5, TokensCachedRead: 3, TokensCachedWrite: 1,
			ReasoningTokens: 2, TokensSaved: 40, CompressionApplied: true, ContextTokensPre: 90, ContextTokensSaved: 7, TTFTms: ms(120)},
		{TS: day.Add(time.Hour), Model: "a", Status: 429, TokensIn: 1, TokensSaved: 25, CompressionApplied: false, TTFTms: ms(300)},
		{TS: day.Add(26 * time.Hour), Model: "b", Status: 200, TokensIn: 7, TokensSaved: 9, CompressionApplied: true, TTFTms: ms(80)},
	}); err != nil {
		t.Fatal(err)
	}
	from, to := day.Add(-time.Hour), day.Add(72*time.Hour)
	sqlRows, err := db.UsageReport(ctx, from, to, "day")
	if err != nil {
		t.Fatal(err)
	}
	tzRows, err := db.UsageDayReportTZ(ctx, from, to, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]UsageRow{}
	for _, r := range sqlRows {
		byKey[r.Key] = r
	}
	if len(tzRows) != len(sqlRows) {
		t.Fatalf("tz rows %d, sql rows %d", len(tzRows), len(sqlRows))
	}
	for _, r := range tzRows {
		if want := byKey[r.Key]; !reflect.DeepEqual(r, want) {
			t.Errorf("day %s:\n tz  %+v\n sql %+v", r.Key, r, want)
		}
	}
	if byKey["2026-10-18"].AppliedSaved != 40 {
		t.Errorf("applied_saved = %d, want 40 (only the applied row)", byKey["2026-10-18"].AppliedSaved)
	}
}

// A healthy meter leaves the unmetered counter alone.
func TestMeteredBatchLeavesUnmeteredRowsUnchanged(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	aid := seedAccountForRule(t, db, "healthy")
	if _, err := db.UpsertModelRule(ctx, ModelRule{AccountID: aid, ModelID: "m1", CapWindow: "daily", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	before := UnmeteredRows()
	if err := db.insertCalls(ctx, []Call{{TS: time.Now().UTC(), AccountID: aid, Model: "m1", TokensIn: 5}}); err != nil {
		t.Fatal(err)
	}
	if got := UnmeteredRows() - before; got != 0 {
		t.Errorf("unmetered_rows delta = %d on a healthy batch, want 0", got)
	}
}
