package store

import (
	"testing"
	"time"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	l, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return l
}

// TestCountBucket pins the bucket format and, more importantly, the timezone
// alignment: a cap day must not straddle two timezones, and a US night rule
// must bucket at New York midnight rather than Kuala Lumpur's.
func TestCountBucket(t *testing.T) {
	kl := mustLoc(t, "Asia/Kuala_Lumpur")
	ny := mustLoc(t, "America/New_York")
	utc := time.UTC

	cases := []struct {
		name   string
		when   time.Time
		window string
		loc    *time.Location
		want   string
	}{
		// 5h calendar blocks: hour/5.
		{"5h block0 midnight", time.Date(2026, 10, 6, 0, 0, 0, 0, kl), "5h", kl, "2026-10-06-T0"},
		{"5h block0 late", time.Date(2026, 10, 6, 4, 59, 0, 0, kl), "5h", kl, "2026-10-06-T0"},
		{"5h block1", time.Date(2026, 10, 6, 5, 0, 0, 0, kl), "5h", kl, "2026-10-06-T1"},
		{"5h block2", time.Date(2026, 10, 6, 14, 37, 0, 0, kl), "5h", kl, "2026-10-06-T2"},
		{"5h block3", time.Date(2026, 10, 6, 15, 0, 0, 0, kl), "5h", kl, "2026-10-06-T3"},
		// block 4 is short (20:00-23:59) — a day has 24h not 25.
		{"5h block4", time.Date(2026, 10, 6, 23, 59, 0, 0, kl), "5h", kl, "2026-10-06-T4"},

		{"daily", time.Date(2026, 10, 6, 23, 59, 0, 0, kl), "daily", kl, "2026-10-06"},
		{"monthly", time.Date(2026, 10, 6, 1, 0, 0, 0, kl), "monthly", kl, "2026-10"},
		{"monthly year boundary", time.Date(2026, 12, 31, 23, 0, 0, 0, kl), "monthly", kl, "2026-12"},

		// ISO week: 2026-01-01 is a Thursday, so it belongs to 2026-W01.
		{"weekly iso", time.Date(2026, 1, 1, 12, 0, 0, 0, utc), "weekly", utc, "2026-W01"},
		// 2025-12-29 is a Monday in ISO week 2026-W01 (ISO weeks start Monday).
		{"weekly iso cross-year", time.Date(2025, 12, 29, 0, 0, 0, 0, utc), "weekly", utc, "2026-W01"},

		// Timezone shift: the same instant buckets to different days in KL vs NY.
		// 2026-10-06 07:00 UTC = 15:00 KL (same day) but 03:00 NY (same day too);
		// pick an instant that crosses midnight in NY but not KL:
		// 2026-10-07 02:00 UTC = 10:00 KL (Oct 7) but 22:00 NY (Oct 6).
		{"tz KL day", time.Date(2026, 10, 7, 2, 0, 0, 0, utc), "daily", kl, "2026-10-07"},
		{"tz NY day", time.Date(2026, 10, 7, 2, 0, 0, 0, utc), "daily", ny, "2026-10-06"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := CountBucket(c.when, c.window, c.loc)
			if got != c.want {
				t.Errorf("CountBucket(%v, %q, %v) = %q, want %q", c.when, c.window, c.loc, got, c.want)
			}
		})
	}
}

// TestCountBucketNilLocDefaultsUTC guards the corrupt-tz path: an unparseable
// zone must not panic the ledger flush, and buckets must stay consistent.
func TestCountBucketNilLocDefaultsUTC(t *testing.T) {
	when := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	if got := CountBucket(when, "daily", nil); got != "2026-10-06" {
		t.Errorf("nil loc: got %q, want 2026-10-06 (UTC)", got)
	}
}
