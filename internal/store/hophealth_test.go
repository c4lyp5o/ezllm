package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// The round-trip is the proof: an INSERT/placeholder/arg mismatch fails here
// immediately, and a column nobody ever writes is caught by the dispatch test
// (internal/server) rather than hiding behind a green read-side assertion.
func TestStreamOutcomeRoundTrip(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "fresh.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if got := db.SchemaVersion(); got != schemaVersion {
		t.Fatalf("schema version = %d, want %d (the constant, not a literal)", got, schemaVersion)
	}

	want := []string{
		StreamOutcomeCompleted,
		StreamOutcomeTruncated,
		StreamOutcomeFailed,
		StreamOutcomeClientGone,
		StreamOutcomeNone,
	}
	for i, outcome := range want {
		db.RecordCall(Call{
			TS: time.Now().UTC().Add(time.Duration(i) * time.Millisecond),
			// every row needs a distinct model so the rollup test below reads
			// clean groups; the outcome is what this test is about
			Client: "c", Account: "ns", Model: "m" + outcome, Status: 200,
			Stream: true, StreamOutcome: outcome,
		})
	}
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}
	rows, err := db.RecentCalls(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %d, want %d", len(rows), len(want))
	}
	// RecentCalls is newest-first, so reverse the want order to compare
	for i, r := range rows {
		exp := want[len(want)-1-i]
		if r.StreamOutcome != exp {
			t.Errorf("row %d stream_outcome = %q, want %q", i, r.StreamOutcome, exp)
		}
	}
}

// A caller that hangs up after the last byte is a COMPLETE call. Counting those
// as truncations is the mislabel that once reported 722 failures where 51 were
// real, so the rate's denominator must exclude them.
func TestHopStreamHealthExcludesClientGoneAndRespectsWindow(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "fresh.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now().UTC()
	add := func(model, outcome string, at time.Time) {
		db.RecordCall(Call{TS: at, Client: "c", Account: "ns", Model: model, Status: 200,
			Stream: true, StreamOutcome: outcome})
	}
	// judged = 10: 8 completed + 2 truncated -> 20%
	for i := 0; i < 8; i++ {
		add("bad-hop", StreamOutcomeCompleted, now.Add(-time.Minute*10))
	}
	add("bad-hop", StreamOutcomeTruncated, now.Add(-time.Minute*9))
	add("bad-hop", StreamOutcomeTruncated, now.Add(-time.Minute*8))
	// 30 client hangups must not dilute or inflate the rate
	for i := 0; i < 30; i++ {
		add("bad-hop", StreamOutcomeClientGone, now.Add(-time.Minute*7))
	}
	// a clean hop, and one row outside the window
	for i := 0; i < 5; i++ {
		add("good-hop", StreamOutcomeCompleted, now.Add(-time.Minute*5))
	}
	add("bad-hop", StreamOutcomeTruncated, now.Add(-48*time.Hour))
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}

	rows, err := db.HopStreamHealth(ctx, now.Add(-24*time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("hops = %d, want 2 (bad-hop, good-hop); got %+v", len(rows), rows)
	}
	// worst first
	if rows[0].Model != "bad-hop" {
		t.Fatalf("first row = %q, want the worst hop first", rows[0].Model)
	}
	bad := rows[0]
	if bad.Streams != 40 || bad.Truncated != 2 || bad.ClientGone != 30 || bad.Completed != 8 {
		t.Fatalf("bad-hop = %+v, want streams=40 truncated=2 client_gone=30 completed=8", bad)
	}
	if got := bad.Rate; got < 0.199 || got > 0.201 {
		t.Fatalf("truncation rate = %.3f, want 0.20 (2 / (40-30)); client_gone must not pad the denominator", got)
	}
	good := rows[1]
	if good.Model != "good-hop" || good.Rate != 0 {
		t.Fatalf("good-hop = %+v, want rate 0", good)
	}
}

// A v33 database (the shipped schema before this change) must migrate up with
// its rows intact. A fresh DB never executes the upgrade branch.
func TestStreamOutcomeMigrationFromV33(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.sqlite")
	db, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	db.RecordCall(Call{TS: time.Now().UTC(), Client: "keeper", Account: "ns",
		Model: "m", Status: 200, Stream: true, StreamOutcome: StreamOutcomeTruncated})
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}
	// Regress to v33: drop the column, stamp the old version.
	if _, err := db.w.ExecContext(ctx, "ALTER TABLE calls DROP COLUMN stream_outcome"); err != nil {
		t.Fatalf("drop column: %v", err)
	}
	if _, err := db.w.ExecContext(ctx, `UPDATE schema_version SET version = 33`); err != nil {
		t.Fatalf("stamp v33: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("reopen/migrate: %v", err)
	}
	defer migrated.Close()
	if got := migrated.SchemaVersion(); got != schemaVersion {
		t.Fatalf("schema version = %d, want %d", got, schemaVersion)
	}
	has, err := hasColumn(ctx, migrated.w, "calls", "stream_outcome")
	if err != nil || !has {
		t.Fatalf("calls.stream_outcome missing after migrate (has=%v err=%v)", has, err)
	}
	// Data preservation, and the honest default: the pre-existing row reads ''
	// (never backfilled) while the keeper row's value is what the OLD binary
	// could not have written, so assert the row survived by its client.
	rows, err := migrated.RecentCalls(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Client != "keeper" {
		t.Fatalf("rows = %+v, want the pre-upgrade row intact", rows)
	}
	if rows[0].StreamOutcome != "" {
		t.Fatalf("pre-upgrade row stream_outcome = %q, want \"\" (never inferred)", rows[0].StreamOutcome)
	}
}
