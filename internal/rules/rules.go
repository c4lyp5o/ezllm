// Package rules is the rules engine: the single eligibility predicate the
// router's Eligible seam calls to decide whether a model may be used RIGHT
// NOW. Two questions live here — the usage cap ("have we spent the budget?")
// and the allowed-hours window ("is it the right time of day?") — behind one
// func(ctx, accountID, modelID) error so routing strategies never need to know
// WHY a hop is unavailable, only that it is.
//
// Design doc: docs/rules-engine-design.md (approved 2026-10-06).
//
// Two invariants this package exists to keep:
//
//  1. The window check does ZERO I/O. It is pure arithmetic over a cached rule
//     set — a time-of-day filter must never add a database round-trip to a
//     request.
//  2. The cap check is ONE primary-key lookup. No SUM() over `calls` ever
//     reaches the hot path (that would scale with the ledger's size, not its
//     rate). The meter is maintained incrementally by the store's ledger
//     flush; here we only read it.
package rules

import (
	"fmt"
	"strings"
	"time"
)

// Clock abstracts "now" so tests inject a fixed instant instead of sleeping
// across a day boundary (the design forbids time.Sleep in boundary tests).
type Clock func() time.Time

// SystemClock is the production clock.
func SystemClock() time.Time { return time.Now() }

// Window is a parsed allowed-hours rule. A nil/empty window (Start=="" &&
// End=="") means "always allowed".
type Window struct {
	Start string // 'HH:MM' local to TZ
	End   string // 'HH:MM'; End < Start wraps midnight (overnight)
	Days  []int  // 0-6 (Sun=0); empty = every day
	TZ    string // IANA name or offset; empty = UTC
}

// Rule is one (account, model) policy — the projection of a model_rules row
// the predicate needs.
type Rule struct {
	AccountID int64
	ModelID   string
	CapTokens int64 // 0 = unlimited
	CapWindow string
	Window    Window
	Enabled   bool
}

// AllowAll reports whether the rule imposes no restriction at all: no cap and
// no window. Such a rule is skipped entirely (no counter read, no arithmetic).
func (r *Rule) AllowAll() bool {
	return r.CapTokens <= 0 && r.Window.Start == "" && r.Window.End == ""
}

// ParseHHMM splits an 'HH:MM' string into hour and minute, rejecting
// anything malformed. Callers must have validated the string at write time
// (ValidateModelRule), but a corrupt row must still fail closed with a clear
// error rather than silently allowing access.
func ParseHHMM(s string) (hour, min int, err error) {
	var h, m int
	if n, e := fmt.Sscanf(s, "%d:%d", &h, &m); e != nil || n != 2 {
		return 0, 0, fmt.Errorf("bad time %q (want HH:MM)", s)
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, fmt.Errorf("time %q out of range", s)
	}
	return h, m, nil
}

// loc resolves the window's timezone, defaulting to UTC when empty or
// unparseable. A corrupt tz must not panic and must not crash the request:
// UTC is the safe neutral because buckets only need to be *consistent* — a
// wrong-but-stable boundary is recoverable, a panic is not.
func (w Window) loc() *time.Location {
	if w.TZ == "" {
		return time.UTC
	}
	if l, err := time.LoadLocation(w.TZ); err == nil {
		return l
	}
	return time.UTC
}

// InWindow reports whether `now` falls inside the allowed-hours window.
//
// The logic that matters:
//
//   - Overnight windows (End < Start, e.g. 22:00→06:00) split across midnight:
//     "in" means now >= Start OR now < End.
//   - Day attribution for an overnight window is anchored to the day the
//     window OPENED, not the calendar day of the request. A request at 01:00
//     Tuesday inside a 22:00→06:00 window belongs to MONDAY's window. Getting
//     this backwards makes "Mon–Fri nights" exclude Friday 01:00 — i.e.
//     Saturday morning, the exact night the rule was written for. We compute
//     the anchor by shifting `now` back by the window's duration when the
//     request lands in the early-morning tail.
//   - The comparison happens in the rule's TZ, never UTC and never the
//     server's local zone: a US vendor's night discount flips at New York
//     midnight, not Kuala Lumpur's (Malaysia is fixed UTC+8 with no DST, so
//     our own default is safe — but a vendor's is not).
//   - Empty Days = every day. Otherwise the anchor day must be in Days.
func (w Window) InWindow(now time.Time) (bool, error) {
	if w.Start == "" && w.End == "" {
		return true, nil // no window = always allowed
	}
	if w.Start == "" || w.End == "" {
		// Half a window is a corrupt row (write validation forbids it).
		// Fail closed: refuse rather than silently allow.
		return false, fmt.Errorf("rule window is half-specified (start=%q end=%q)", w.Start, w.End)
	}

	sh, sm, err := ParseHHMM(w.Start)
	if err != nil {
		return false, err
	}
	eh, em, err := ParseHHMM(w.End)
	if err != nil {
		return false, err
	}

	loc := w.loc()
	lt := now.In(loc)
	nowM := lt.Hour()*60 + lt.Minute()
	startM := sh*60 + sm
	endM := eh*60 + em

	// Determine whether this instant is inside the time-of-day span, and the
	// anchor day it should be attributed to for the weekday filter.
	var inSpan bool
	anchor := lt
	if startM < endM {
		// Same-day window (e.g. 09:00→17:00): in = start <= now < end.
		inSpan = nowM >= startM && nowM < endM
		// anchor stays today.
	} else if startM > endM {
		// Overnight window (e.g. 22:00→06:00): spans midnight. "In" is the
		// union of the tail of one day and the head of the next.
		inSpan = nowM >= startM || nowM < endM
		if nowM < endM {
			// We're in the early-morning TAIL: the window opened YESTERDAY.
			// Shift the anchor back one day so the weekday filter sees the
			// day the window started, not the calendar day we're in.
			anchor = lt.AddDate(0, 0, -1)
		}
	} else {
		// start == end: an empty window (write validation rejects this, but a
		// corrupt row must fail closed, not read as all-day).
		return false, fmt.Errorf("rule window start==end (%q)", w.Start)
	}

	if !inSpan {
		return false, nil
	}

	if len(w.Days) == 0 {
		return true, nil // every day
	}
	want := int(anchor.Weekday()) // Sunday == 0, matching win_days
	for _, d := range w.Days {
		if d == want {
			return true, nil
		}
	}
	// Inside the time span but not on an allowed day.
	return false, nil
}

// NextAllowed returns the next instant (in the rule's TZ) at which the window
// opens. It powers the 429 body's `next_allowed` so a client can sleep until
// then instead of blind-retrying. Returns the zero time when there is no
// window (nothing to wait for).
//
// It only handles the common "wait for the next window start" case: if we are
// inside the span but on a wrong day, or outside, we advance to the next
// occurrence of Start. This is a heuristic for a client backoff, not a full
// calendar solver — good enough to say "come back at 22:00".
func (w Window) NextAllowed(now time.Time) time.Time {
	if w.Start == "" {
		return time.Time{}
	}
	sh, sm, err := ParseHHMM(w.Start)
	if err != nil {
		return time.Time{}
	}
	loc := w.loc()
	lt := now.In(loc)
	// Candidate: today at Start.
	next := time.Date(lt.Year(), lt.Month(), lt.Day(), sh, sm, 0, 0, loc)
	if !next.After(now) {
		// Start already passed (or is now): next day at Start.
		next = next.AddDate(0, 0, 1)
	}
	return next
}

// bucketFor returns the meter bucket key for `now` under the rule's cap
// window, aligned to the rule's timezone. This mirrors store.CountBucket —
// the store writes counters with the same key, so read and write always agree.
// (The rule owns its tz here rather than trusting a global: two rules can
// meter against different timezones.)
func (r *Rule) bucketFor(now time.Time, loadLoc func(string) *time.Location) string {
	loc := time.UTC
	if loadLoc != nil {
		if l := loadLoc(r.Window.TZ); l != nil {
			loc = l
		}
	} else if r.Window.TZ != "" {
		if l, err := time.LoadLocation(r.Window.TZ); err == nil {
			loc = l
		}
	}
	lt := now.In(loc)
	switch r.CapWindow {
	case "5h":
		return fmt.Sprintf("%s-T%d", lt.Format("2006-01-02"), lt.Hour()/5)
	case "daily":
		return lt.Format("2006-01-02")
	case "weekly":
		y, wk := lt.ISOWeek()
		return fmt.Sprintf("%04d-W%02d", y, wk)
	default: // "monthly", ""
		return lt.Format("2006-01")
	}
}

// summarizeDays turns a day list into a short human string for error reasons,
// e.g. "Mon-Fri" collapses when contiguous; otherwise "Sun,Tue".
func summarizeDays(days []int) string {
	if len(days) == 0 {
		return ""
	}
	names := [7]string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}
	present := [7]bool{}
	for _, d := range days {
		if d >= 0 && d <= 6 {
			present[d] = true
		}
	}
	var parts []string
	for i := 0; i < 7; i++ {
		if present[i] {
			parts = append(parts, names[i])
		}
	}
	return strings.Join(parts, ",")
}
