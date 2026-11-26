package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/settings"
	"github.com/c4lyp5o/ezllm/internal/store"
)

func pct(v float64) *float64 { return &v }

// A window at or over the threshold warns once per window; under it, or with
// the switch off, nothing.
func TestQuotaAlertThresholdAndWindow(t *testing.T) {
	v := settings.Defaults()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	q := provider.Quota{Kind: provider.QuotaPercent, PercentRolling: pct(40), PercentWeekly: pct(85), WeeklyResetsAt: "2026-11-30T00:00:00Z", PercentMonthly: pct(80)}
	got := quotaAlerts(7, "acme", q, v, now)
	if len(got) != 2 {
		t.Fatalf("alerts = %+v, want weekly and monthly (80%% counts)", got)
	}
	if !strings.Contains(got[0].Title, "85% of its weekly quota") || got[0].DedupKey != "quota_warning:7:weekly:2026-11-30T00:00:00Z" || !got[0].Once {
		t.Fatalf("weekly alert = %+v", got[0])
	}
	// No reset time: the window is the calendar month.
	if got[1].DedupKey != "quota_warning:7:monthly:2026-11" {
		t.Fatalf("monthly dedup = %q", got[1].DedupKey)
	}
	v.AlertQuotaPct = 90
	if got := quotaAlerts(7, "acme", q, v, now); len(got) != 0 {
		t.Fatalf("under a 90%% threshold: %+v", got)
	}
	v = settings.Defaults()
	v.NotifyQuota = false
	if got := quotaAlerts(7, "acme", q, v, now); len(got) != 0 {
		t.Fatalf("switched off: %+v", got)
	}
}

// A balance under the threshold warns once a day.
func TestBalanceAlert(t *testing.T) {
	v := settings.Defaults()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	low := provider.Quota{Kind: provider.QuotaMoney, Balance: pct(1.5), BalanceUnit: "USD"}
	got := quotaAlerts(7, "acme", low, v, now)
	if len(got) != 1 || got[0].Kind != store.NoteBalanceLow || !strings.Contains(got[0].Title, "1.50 USD") || got[0].DedupKey != "balance_low:7:2026-11-26" {
		t.Fatalf("low balance = %+v", got)
	}
	if got := quotaAlerts(7, "acme", provider.Quota{Kind: provider.QuotaMoney, Balance: pct(2)}, v, now); len(got) != 0 {
		t.Fatalf("2.00 is not under 2.00: %+v", got)
	}
}

// A cookie expiring within the window warns; one already expired is an error.
func TestCookieAlert(t *testing.T) {
	v := settings.Defaults()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	if _, due := cookieAlert(7, "acme", now.Add(72*time.Hour), v, now); due {
		t.Fatal("72 hours out is not within 48")
	}
	in, due := cookieAlert(7, "acme", now.Add(30*time.Hour), v, now)
	if !due || in.Severity != "warn" || !strings.Contains(in.Title, "30 hours") || !in.Once {
		t.Fatalf("30 hours out = %+v %v", in, due)
	}
	in, _ = cookieAlert(7, "acme", now.Add(-time.Hour), v, now)
	if in.Severity != "error" || !strings.Contains(in.Title, "has expired") {
		t.Fatalf("expired = %+v", in)
	}
	v.NotifyCookie = false
	if _, due := cookieAlert(7, "acme", now.Add(time.Hour), v, now); due {
		t.Fatal("switched off")
	}
}

// The hourly check reads the quota, saves it, and says a low balance once,
// however many times it runs.
func TestAlertCheckSaysLowBalanceOnce(t *testing.T) {
	fake := &fakeUpstream{models: []string{"m"}, quotaBody: `{"balance":1.25,"unit":"USD"}`}
	h := settingsHarness(t, fake.handler())
	for i := 0; i < 3; i++ {
		h.server.checkAlerts(context.Background(), time.Now())
	}
	var rows, count, snaps int
	if err := h.db.Reader().QueryRow(`SELECT COUNT(*), COALESCE(SUM(count),0) FROM notifications WHERE kind='balance_low'`).Scan(&rows, &count); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || count != 1 {
		t.Fatalf("balance_low rows=%d count=%d, want one row said once", rows, count)
	}
	if err := h.db.Reader().QueryRow(`SELECT COUNT(*) FROM quota_snapshots WHERE kind='money'`).Scan(&snaps); err != nil {
		t.Fatal(err)
	}
	if snaps != 3 {
		t.Fatalf("quota reads saved = %d, want 3", snaps)
	}
}

// Settings refuse a threshold outside its range.
func TestAlertSettingsValidate(t *testing.T) {
	for _, bad := range []func(*settings.Values){
		func(v *settings.Values) { v.AlertQuotaPct = 0 },
		func(v *settings.Values) { v.AlertQuotaPct = 101 },
		func(v *settings.Values) { v.AlertBalance = 0 },
		func(v *settings.Values) { v.AlertCookieHours = 0 },
	} {
		v := settings.Defaults()
		bad(&v)
		if v.Validate() == nil {
			t.Errorf("accepted %+v", v)
		}
	}
	if err := settings.Defaults().Validate(); err != nil {
		t.Fatal(err)
	}
}

// Turning the balance warning off in Settings silences it.
func TestAlertCheckRespectsTheSwitch(t *testing.T) {
	fake := &fakeUpstream{models: []string{"m"}, quotaBody: `{"balance":1.25}`}
	h := settingsHarness(t, fake.handler())
	m := h.server.settings
	if _, err := m.Patch(context.Background(), m.View().Revision, map[string]json.RawMessage{"notify_balance_low": json.RawMessage("false")}); err != nil {
		t.Fatal(err)
	}
	h.server.checkAlerts(context.Background(), time.Now())
	var rows int
	_ = h.db.Reader().QueryRow(`SELECT COUNT(*) FROM notifications WHERE kind='balance_low'`).Scan(&rows)
	if rows != 0 {
		t.Fatalf("balance_low rows = %d with the switch off", rows)
	}
}

// A provider whose usage page fails is logged at Info (routine, tried again
// next hour) and filtered out at Warn; a broken database is a Warn.
func TestAlertCheckLogLines(t *testing.T) {
	upstream := func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/usage") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		(&fakeUpstream{models: []string{"m"}}).handler()(w, r)
	}
	for _, tc := range []struct {
		level slog.Level
		want  bool
	}{{slog.LevelInfo, true}, {slog.LevelWarn, false}} {
		var buf bytes.Buffer
		h := newHarness(t, upstream, func(o *Options) {
			m, err := settings.New(context.Background(), o.DB, settings.Defaults(), nil)
			if err != nil {
				t.Fatal(err)
			}
			o.Settings = m
			o.Log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: tc.level}))
		})
		h.server.checkAlerts(context.Background(), time.Now())
		if got := strings.Contains(buf.String(), "alert check: quota read failed"); got != tc.want {
			t.Fatalf("level %v: logged=%v, want %v\n%s", tc.level, got, tc.want, buf.String())
		}
		if strings.Contains(buf.String(), "sk-upstream-key") {
			t.Fatal("the key reached the log")
		}
	}
	var buf bytes.Buffer
	h := newHarness(t, upstream, func(o *Options) {
		m, err := settings.New(context.Background(), o.DB, settings.Defaults(), nil)
		if err != nil {
			t.Fatal(err)
		}
		o.Settings = m
		o.Log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	})
	_ = h.db.Close()
	h.server.checkAlerts(context.Background(), time.Now())
	if !strings.Contains(buf.String(), `msg="alert check failed" step="prune quota reads"`) {
		t.Fatalf("a closed database was not logged at Warn:\n%s", buf.String())
	}
}
