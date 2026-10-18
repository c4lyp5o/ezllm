package server

import (
	"testing"
	"time"
)

func TestNormalizeEffective(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	got, err := normalizeEffective("", now)
	if err != nil || got != "2026-10-18" {
		t.Fatalf("empty = %q, %v; want 2026-10-18, nil", got, err)
	}
	got, err = normalizeEffective("2026-09-18", now)
	if err != nil || got != "2026-09-18" {
		t.Fatalf("date = %q, %v; want 2026-09-18, nil", got, err)
	}
	if _, err := normalizeEffective("18-09-2026", now); err == nil {
		t.Fatal("bad date accepted")
	}
}

func TestNextDailyUsesLocalHour(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Kuala_Lumpur")
	if err != nil {
		t.Skip("tz database unavailable:", err)
	}
	next := nextDaily(3, loc)
	if next.Hour() != 3 || next.Minute() != 0 {
		t.Fatalf("next = %s; want 03:00 local", next)
	}
	if !next.After(time.Now()) {
		t.Fatalf("next = %s; not in the future", next)
	}
}
