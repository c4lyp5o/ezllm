package rules

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

// at builds a wall-clock time in loc — the way a rule author thinks ("22:00
// in KL"), independent of any instant.
func at(loc *time.Location, y int, m time.Month, d, hh, mm int) time.Time {
	return time.Date(y, m, d, hh, mm, 0, 0, loc)
}

func TestInWindowNormalDay(t *testing.T) {
	kl := mustLoc(t, "Asia/Kuala_Lumpur")
	w := Window{Start: "09:00", End: "17:00", TZ: "Asia/Kuala_Lumpur"}

	cases := []struct {
		when time.Time
		want bool
	}{
		{at(kl, 2026, 10, 6, 8, 59), false}, // before start
		{at(kl, 2026, 10, 6, 9, 0), true},   // exactly at start (inclusive)
		{at(kl, 2026, 10, 6, 12, 0), true},  // midday
		{at(kl, 2026, 10, 6, 16, 59), true}, // just before end
		{at(kl, 2026, 10, 6, 17, 0), false}, // end is exclusive
		{at(kl, 2026, 10, 6, 23, 0), false}, // evening
	}
	for _, c := range cases {
		got, err := w.InWindow(c.when)
		if err != nil {
			t.Fatalf("InWindow(%v): %v", c.when, err)
		}
		if got != c.want {
			t.Errorf("InWindow(%v) = %v, want %v", c.when, got, c.want)
		}
	}
}

// TestInWindowOvernight is the case the design called out as genuinely
// fiddly: a 22:00→06:00 window spanning midnight, with day attribution
// anchored to the day the window OPENED.
func TestInWindowOvernight(t *testing.T) {
	kl := mustLoc(t, "Asia/Kuala_Lumpur")
	w := Window{Start: "22:00", End: "06:00", TZ: "Asia/Kuala_Lumpur"}

	cases := []struct {
		name string
		when time.Time
		want bool
	}{
		{"just before start", at(kl, 2026, 10, 6, 21, 59), false},
		{"at start", at(kl, 2026, 10, 6, 22, 0), true},
		{"late evening", at(kl, 2026, 10, 6, 23, 30), true},
		{"midnight", at(kl, 2026, 10, 7, 0, 0), true},
		{"early morning tail", at(kl, 2026, 10, 7, 3, 0), true},
		{"just before end", at(kl, 2026, 10, 7, 5, 59), true},
		{"at end (exclusive)", at(kl, 2026, 10, 7, 6, 0), false},
		{"morning", at(kl, 2026, 10, 7, 8, 0), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := w.InWindow(c.when)
			if err != nil {
				t.Fatalf("InWindow: %v", err)
			}
			if got != c.want {
				t.Errorf("InWindow(%v) = %v, want %v", c.when, got, c.want)
			}
		})
	}
}

// TestInWindowOvernightDayAnchoring pins the trap: a request at 01:00 Tuesday
// inside a Mon-Fri 22:00→06:00 window belongs to MONDAY's window. The window
// opened Monday 22:00, so the weekday filter must see Monday (1), not Tuesday
// (2). Getting this backwards makes Friday 01:00 fall on Saturday — the exact
// night the rule was written for.
func TestInWindowOvernightDayAnchoring(t *testing.T) {
	kl := mustLoc(t, "Asia/Kuala_Lumpur")
	// Mon-Fri only (win_days 1,2,3,4,5).
	w := Window{Start: "22:00", End: "06:00", Days: []int{1, 2, 3, 4, 5}, TZ: "Asia/Kuala_Lumpur"}

	// 2026-10-05 is a Monday. So:
	//   Mon 22:00  → in (Mon, day 1) ✓
	//   Tue 01:00  → tail of Mon's window (anchor Mon, day 1) ✓
	//   Fri 23:00  → in (Fri, day 5) ✓
	//   Sat 03:00  → tail of Fri's window (anchor Fri, day 5) ✓ — the trap!
	//   Sat 23:00  → in? anchor Sat (day 6) ✗ (not a weekday)
	//   Sun 03:00  → tail of Sat's window (anchor Sat, day 6) ✗
	cases := []struct {
		name string
		when time.Time
		want bool
	}{
		{"Mon 22:00 open", at(kl, 2026, 10, 5, 22, 0), true},
		{"Tue 01:00 anchors Mon", at(kl, 2026, 10, 6, 1, 0), true},
		{"Fri 23:00 open", at(kl, 2026, 10, 9, 23, 0), true},
		{"Sat 03:00 anchors Fri (the trap)", at(kl, 2026, 10, 10, 3, 0), true},
		{"Sat 23:00 anchors Sat (not allowed)", at(kl, 2026, 10, 10, 23, 0), false},
		{"Sun 03:00 anchors Sat (not allowed)", at(kl, 2026, 10, 11, 3, 0), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := w.InWindow(c.when)
			if err != nil {
				t.Fatalf("InWindow: %v", err)
			}
			if got != c.want {
				t.Errorf("InWindow(%v) = %v, want %v", c.when, got, c.want)
			}
		})
	}
}

// TestInWindowTimezoneFlip: the same instant must be judged in the RULE's
// timezone, not UTC and not the server's. A US night rule flips at New York
// midnight even though it is still the previous afternoon in KL.
func TestInWindowTimezoneFlip(t *testing.T) {
	w := Window{Start: "22:00", End: "06:00", TZ: "America/New_York"}

	// 2026-10-06 10:00 UTC = 06:00 NY (just at end, out) = 18:00 KL.
	sameInstant := time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC)
	got, err := w.InWindow(sameInstant)
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Errorf("10:00 UTC = 06:00 NY should be OUT of 22:00-06:00 NY, got in=true")
	}

	// 2026-10-07 03:00 UTC = 23:00 NY Oct 6 (in) = 11:00 KL Oct 7.
	instant := time.Date(2026, 10, 7, 3, 0, 0, 0, time.UTC)
	got, err = w.InWindow(instant)
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Errorf("03:00 UTC = 23:00 NY should be IN, got in=false (KL would say otherwise — rule tz must win)")
	}
}

// TestInWindowDaysFilter: a same-day window restricted to specific days uses
// the calendar day directly (no overnight anchoring).
func TestInWindowDaysFilter(t *testing.T) {
	kl := mustLoc(t, "Asia/Kuala_Lumpur")
	// Mon-Fri 09:00-17:00.
	w := Window{Start: "09:00", End: "17:00", Days: []int{1, 2, 3, 4, 5}, TZ: "Asia/Kuala_Lumpur"}
	// 2026-10-10 is a Saturday.
	got, err := w.InWindow(at(kl, 2026, 10, 10, 12, 0))
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Error("Saturday midday should be OUT of a Mon-Fri window")
	}
	// 2026-10-09 is a Friday.
	got, err = w.InWindow(at(kl, 2026, 10, 9, 12, 0))
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("Friday midday should be IN a Mon-Fri window")
	}
}

// TestInWindowNoWindow: an empty window allows everything.
func TestInWindowNoWindow(t *testing.T) {
	w := Window{}
	got, err := w.InWindow(time.Now())
	if err != nil || !got {
		t.Errorf("empty window should allow, got %v/%v", got, err)
	}
}

// TestInWindowCorruptRowsFailClosed: a half-specified or start==end window is
// a corrupt row (write validation forbids it). It must REFUSE, not silently
// allow — a broken rule granting access is worse than one denying it.
func TestInWindowCorruptRowsFailClosed(t *testing.T) {
	cases := []Window{
		{Start: "09:00"},               // half
		{End: "17:00"},                 // half
		{Start: "09:00", End: "09:00"}, // empty
		{Start: "noon", End: "17:00"},  // malformed start
		{Start: "09:00", End: "25:00"}, // out-of-range end
	}
	for _, w := range cases {
		got, err := w.InWindow(time.Now())
		if err == nil {
			t.Errorf("corrupt window %+v: want error (fail closed), got err=nil in=%v", w, got)
		}
		if got {
			t.Errorf("corrupt window %+v: must not be ALLOWED", w)
		}
	}
}

func TestNextAllowed(t *testing.T) {
	kl := mustLoc(t, "Asia/Kuala_Lumpur")
	w := Window{Start: "22:00", End: "06:00", TZ: "Asia/Kuala_Lumpur"}

	// Before start today → today 22:00.
	now := at(kl, 2026, 10, 6, 12, 0)
	next := w.NextAllowed(now)
	want := at(kl, 2026, 10, 6, 22, 0)
	if !next.Equal(want) {
		t.Errorf("NextAllowed(12:00) = %v, want %v", next, want)
	}

	// After start (window is open) → tomorrow 22:00 (next open).
	now = at(kl, 2026, 10, 6, 23, 0)
	next = w.NextAllowed(now)
	want = at(kl, 2026, 10, 7, 22, 0)
	if !next.Equal(want) {
		t.Errorf("NextAllowed(23:00) = %v, want %v", next, want)
	}

	// No window → zero time (nothing to wait for).
	if !(Window{}).NextAllowed(now).IsZero() {
		t.Error("windowless rule should return zero time")
	}
}
