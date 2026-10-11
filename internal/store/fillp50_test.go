package store

// F5 regression: fillP50 used to SELECT every (k, ttft_ms) row in the range
// into a Go map on every /admin/usage request — cost scaling with ledger size
// rather than request rate, against the codebase's own rule to compute
// aggregates over the full filtered set IN SQL. It now partitions and orders
// with window functions inside SQLite and reads back one row per group.
//
// These tests pin the numbers the dashboard shows: the median must be the
// middle value for an odd count and the UPPER middle for an even count (the
// convention the old Go code implemented silently), and a group with no
// measurable TTFT must keep a zero rather than borrowing a neighbour's.

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), Options{
		Path:      filepath.Join(t.TempDir(), "ledger.sqlite"),
		BatchSize: 128,
		BatchWait: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestFillP50MedianOddAndEven(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	day := "2026-10-11T12:00:00.000Z"
	// acct "odd": 10,30,50,70,90 → middle (3rd of 5) is 50.
	for _, ttft := range []int64{10, 30, 50, 70, 90} {
		db.RecordCall(Call{TS: parseTS(t, day), Client: "c", Account: "odd", AccountID: 1,
			Model: "m1", Status: 200, TTFTms: i64ptr(ttft)})
	}
	// acct "even": 100,200 → upper middle is 200 (the documented convention,
	// exactly what the old vs[len(vs)/2] returned).
	for _, ttft := range []int64{100, 200} {
		db.RecordCall(Call{TS: parseTS(t, day), Client: "c", Account: "even", AccountID: 2,
			Model: "m1", Status: 200, TTFTms: i64ptr(ttft)})
	}
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}

	from, to := parseTS(t, "2026-10-11T00:00:00.000Z"), parseTS(t, "2026-10-12T00:00:00.000Z")
	rows, err := db.UsageReport(ctx, from, to, "account")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, r := range rows {
		got[r.Key] = r.P50TTFTms
	}
	if got["odd"] != 50 {
		t.Errorf("odd group (10,30,50,70,90) p50 = %d, want 50", got["odd"])
	}
	if got["even"] != 200 {
		t.Errorf("even group (100,200) p50 = %d, want 200 — the upper middle, matching the old Go convention", got["even"])
	}
}

// A group whose calls carry no TTFT measurement must keep 0 rather than
// picking up another group's median.
func TestFillP50LeavesUnmeasuredGroupsAtZero(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	day := "2026-10-11T12:00:00.000Z"
	db.RecordCall(Call{TS: parseTS(t, day), Client: "c", Account: "measured", AccountID: 1, Model: "m", Status: 200, TTFTms: i64ptr(42)})
	db.RecordCall(Call{TS: parseTS(t, day), Client: "c", Account: "silent", AccountID: 2, Model: "m", Status: 200}) // TTFTms nil
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}

	from, to := parseTS(t, "2026-10-11T00:00:00.000Z"), parseTS(t, "2026-10-12T00:00:00.000Z")
	rows, err := db.UsageReport(ctx, from, to, "account")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		switch r.Key {
		case "measured":
			if r.P50TTFTms != 42 {
				t.Errorf("measured p50 = %d, want 42", r.P50TTFTms)
			}
		case "silent":
			if r.P50TTFTms != 0 {
				t.Errorf("unmeasured group p50 = %d, want 0", r.P50TTFTms)
			}
		}
	}
}

// The window-function form returns one row per group regardless of how many
// calls each group holds, so the Go side of the query stays bounded.
func TestFillP50IsBoundedByGroupCount(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	day := "2026-10-11T12:00:00.000Z"
	// 200 calls in ONE group: the old shape pulled 200 rows into byKey.
	for i := 0; i < 200; i++ {
		db.RecordCall(Call{TS: parseTS(t, day), Client: "c", Account: "solo", AccountID: 1,
			Model: "m", Status: 200, TTFTms: i64ptr(int64(i + 1))})
	}
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}

	from, to := parseTS(t, "2026-10-11T00:00:00.000Z"), parseTS(t, "2026-10-12T00:00:00.000Z")
	rows, err := db.UsageReport(ctx, from, to, "account")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("usage groups = %d, want 1", len(rows))
	}
	if rows[0].Calls != 200 {
		t.Errorf("calls = %d, want 200", rows[0].Calls)
	}
	// 200 values 1..200 → the old vs[len(vs)/2] took index 100, i.e. the
	// 101st value.
	if rows[0].P50TTFTms != 101 {
		t.Errorf("p50 = %d, want 101 (upper middle of 1..200)", rows[0].P50TTFTms)
	}
}

func parseTS(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse("2006-01-02T15:04:05.000Z", s)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func i64ptr(v int64) *int64 { return &v }
