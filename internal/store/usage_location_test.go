package store

import (
	"context"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/settings"
)

// Client-token budgets and date-only expiries follow the effective usage
// timezone, not the process TZ, so one timezone setting drives every
// day boundary an operator sees.
func TestClientBudgetsFollowTheUsageTimezone(t *testing.T) {
	d := openTest(t)
	if got := d.usageLocation(); got != time.UTC {
		t.Fatalf("no settings = %v, want UTC", got)
	}
	v := settings.Defaults()
	v.UsageTZ = "Pacific/Kiritimati" // UTC+14: a different calendar day from UTC half the time
	m, err := settings.New(context.Background(), d, v, nil)
	if err != nil {
		t.Fatal(err)
	}
	d.SetOperatorSettings(m)
	loc := d.usageLocation()
	if loc.String() != "Pacific/Kiritimati" {
		t.Fatalf("usage location = %v", loc)
	}
	ts := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC) // already Nov 16 in Kiritimati
	if CountBucket(ts, "daily", loc) == CountBucket(ts, "daily", time.UTC) {
		t.Fatal("daily bucket ignored the usage timezone")
	}
	g := groupForClientCounter([]Call{{Client: "c", TS: ts}}, map[string]windowSpec{"c": {window: "daily"}}, loc)
	if len(g) != 1 || g[0].bucket != CountBucket(ts, "daily", loc) {
		t.Fatalf("metered bucket = %+v, want the usage-timezone day", g)
	}
	exp, err := normalizeExpiry("2026-11-20", loc)
	if err != nil || exp != "2026-11-20T09:59:59Z" {
		t.Fatalf("date-only expiry = %q %v, want the end of Nov 20 in Kiritimati", exp, err)
	}
}
