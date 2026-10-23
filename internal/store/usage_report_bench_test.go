package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// seedLedger inserts n synthetic calls spread over the 7 days from start:
// five accounts, ten models, TTFTs 0-999 ms, every 17th call an error.
func seedLedger(tb testing.TB, d *DB, n int, start time.Time) {
	tb.Helper()
	_, err := d.w.Exec(`WITH RECURSIVE seq(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM seq WHERE n < ?)
INSERT INTO calls(ts, client, surface, account, model, status, tokens_in, tokens_out, ttft_ms,
                  tokens_saved, compression_applied)
SELECT strftime('%Y-%m-%dT%H:%M:%fZ', ?, '+' || ((n * 7919) % 604800) || ' seconds'),
       'client', 'openai', 'account-' || (n % 5), 'model-' || (n % 10),
       CASE WHEN n % 17 = 0 THEN 500 ELSE 200 END, 100, 20,
       CASE WHEN n % 13 = 0 THEN NULL ELSE n % 1000 END,
       n % 7, n % 2
FROM seq`, n, start.UTC().Format("2006-01-02T15:04:05"))
	if err != nil {
		tb.Fatal(err)
	}
}

// BenchmarkUsageDayReportTZ is the dashboard's day breakdown in a non-UTC
// zone over a week of 50,000 calls.
func BenchmarkUsageDayReportTZ(b *testing.B) {
	ctx := context.Background()
	d, err := Open(ctx, Options{Path: filepath.Join(b.TempDir(), "bench.sqlite")})
	if err != nil {
		b.Fatal(err)
	}
	defer d.Close()
	start := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	seedLedger(b, d, 50000, start)
	loc, err := time.LoadLocation("Asia/Kuala_Lumpur")
	if err != nil {
		b.Fatal(err)
	}
	from, to := start, start.Add(7*24*time.Hour)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := d.UsageDayReportTZ(ctx, from, to, loc); err != nil {
			b.Fatal(err)
		}
	}
}

// The SQL day report equals the row-scan reference field for field, across
// zones with DST (including Havana, where DST skips midnight itself) and
// windows that start and end mid-day.
func TestUsageDayReportTZMatchesRowScan(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		zone  string
		start time.Time // seeded week begins here
	}{
		{"UTC", time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)},
		{"Asia/Kuala_Lumpur", time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)},
		{"America/New_York", time.Date(2026, 3, 5, 0, 0, 0, 0, time.UTC)},   // spring forward 03-08
		{"America/New_York", time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)}, // fall back 11-01
		{"America/Havana", time.Date(2026, 3, 5, 0, 0, 0, 0, time.UTC)},     // DST at 00:00
	} {
		loc, err := time.LoadLocation(tc.zone)
		if err != nil {
			t.Skipf("no tzdata for %s: %v", tc.zone, err)
		}
		d, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "p.sqlite")})
		if err != nil {
			t.Fatal(err)
		}
		seedLedger(t, d, 3000, tc.start)
		for _, w := range [][2]time.Time{
			{tc.start, tc.start.Add(7 * 24 * time.Hour)},
			{tc.start.Add(13*time.Hour + 17*time.Minute), tc.start.Add(5*24*time.Hour + 9*time.Hour)},
			{tc.start.Add(30 * 24 * time.Hour), tc.start.Add(31 * 24 * time.Hour)}, // no rows
		} {
			got, err := d.UsageDayReportTZ(ctx, w[0], w[1], loc)
			if err != nil {
				t.Fatal(err)
			}
			want, err := d.usageDayReportTZScan(ctx, w[0], w[1], loc)
			if err != nil {
				t.Fatal(err)
			}
			if w[0].Equal(tc.start) && len(want) < 7 {
				t.Fatalf("%s: seeded week yields %d days, want at least 7 (vacuous comparison)", tc.zone, len(want))
			}
			if len(got) != len(want) {
				t.Fatalf("%s %v: %d days, want %d\n got %+v\nwant %+v", tc.zone, w, len(got), len(want), got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("%s %v day %d:\n got %+v\nwant %+v", tc.zone, w, i, got[i], want[i])
				}
			}
		}
		d.Close()
	}
}

// usageDayReportTZScan is the original row-scan UsageDayReportTZ: every call's
// timestamp parsed and bucketed with CountBucket. Kept as the reference the
// production form is tested against.
func (d *DB) usageDayReportTZScan(ctx context.Context, from, to time.Time, loc *time.Location) ([]UsageRow, error) {
	rs, err := d.r.QueryContext(ctx, `
SELECT ts, ttft_ms,
       COALESCE(tokens_in,0), COALESCE(tokens_out,0),
       COALESCE(tokens_cached_read,0), COALESCE(tokens_cached_write,0),
       COALESCE(reasoning_tokens,0), COALESCE(tokens_saved,0),
       COALESCE(context_tokens_pre,0), COALESCE(context_tokens_saved,0),
       status, COALESCE(compression_applied,0)
FROM calls
WHERE ts >= ? AND ts < ?`, from.UTC().Format("2006-01-02T15:04:05.000Z"), to.UTC().Format("2006-01-02T15:04:05.000Z"))
	if err != nil {
		return nil, err
	}
	defer rs.Close()

	type acc struct {
		row   UsageRow
		ttfts []int64
	}
	byDay := map[string]*acc{}
	for rs.Next() {
		var ts string
		var ttft sql.NullInt64
		var tin, tout, cread, cwrite, reasoning, saved, cpre, csaved int64
		var status, applied int
		if err := rs.Scan(&ts, &ttft, &tin, &tout, &cread, &cwrite, &reasoning, &saved, &cpre, &csaved, &status, &applied); err != nil {
			return nil, err
		}
		t, err := time.Parse("2006-01-02T15:04:05.000Z", ts)
		if err != nil {
			continue // a corrupt stamp must not abort the whole report
		}
		day := CountBucket(t, "daily", loc) // the meter's own bucketing
		a := byDay[day]
		if a == nil {
			a = &acc{row: UsageRow{Key: day}}
			byDay[day] = a
		}
		a.row.Calls++
		a.row.TokensIn += tin
		a.row.TokensOut += tout
		a.row.CachedRead += cread
		a.row.CachedWrite += cwrite
		a.row.Reasoning += reasoning
		a.row.TokensSaved += saved
		if applied == 1 {
			a.row.AppliedSaved += saved // the SQL path's applied_saved, row for row
		}
		a.row.ContextPre += cpre
		a.row.ContextSaved += csaved
		if status >= 400 {
			a.row.Errors++
		}
		if ttft.Valid {
			a.ttfts = append(a.ttfts, ttft.Int64)
		}
	}
	if err := rs.Err(); err != nil {
		return nil, err
	}

	out := make([]UsageRow, 0, len(byDay))
	for _, a := range byDay {
		if len(a.ttfts) > 0 {
			slices.Sort(a.ttfts)
			// Same convention as fillP50: the UPPER of the two middle values
			// for an even count. Documented there; kept identical here so the
			// tz path and the SQL path report the same statistic.
			a.row.P50TTFTms = a.ttfts[len(a.ttfts)/2]
		}
		out = append(out, a.row)
	}
	slices.SortFunc(out, func(x, y UsageRow) int { return strings.Compare(x.Key, y.Key) })
	return out, nil
}
