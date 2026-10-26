package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// The authoritative savings aggregate (feasibility audit 2026-10-16):
// UsageReport.AppliedSaved must count ONLY rows where compression_applied=1.
// Legacy rows (tokens_saved>0 on a declined row, written before the
// applied/notional split) stay in TokensSaved as raw evidence but must never
// inflate the headline.

func TestUsageReportAppliedSavedGatesLegacyRows(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "t.sqlite"), BatchSize: 1, BatchWait: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	rows := []Call{
		{TS: now, Client: "c", Surface: "chat", Account: "a", Model: "m",
			Status: 200, TokensIn: 100, TokensSaved: 40, CompressionApplied: true, CompressionProfile: "p"},
		// legacy declined row: saved recorded but nothing shipped
		{TS: now, Client: "c", Surface: "chat", Account: "a", Model: "m",
			Status: 200, TokensIn: 100, TokensSaved: 21, TokensSavedNotional: 21, CompressionApplied: false, CompressionProfile: "p"},
		// clean declined row: only the notional travels
		{TS: now, Client: "c", Surface: "chat", Account: "a", Model: "m",
			Status: 200, TokensIn: 100, TokensSaved: 0, TokensSavedNotional: 9, CompressionApplied: false, CompressionProfile: "p"},
	}
	for _, c := range rows {
		db.RecordCall(c)
	}
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}
	got, err := db.UsageReport(ctx, now.Add(-time.Minute), now.Add(time.Minute), "account")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("groups = %d", len(got))
	}
	r := got[0]
	if r.AppliedSaved != 40 {
		t.Errorf("applied_saved = %d, want 40 (the one shipped rewrite only)", r.AppliedSaved)
	}
	if r.TokensSaved != 61 {
		t.Errorf("raw saved = %d, want 61 (evidence preserved: 40+21)", r.TokensSaved)
	}
	if r.AppliedSaved == r.TokensSaved {
		t.Error("the legacy row must make raw and applied DIFFER — if they are equal the gate is gone")
	}
}
