package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	got := quotaAlerts(7, "acme", q, v, now, "")
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
	if got := quotaAlerts(7, "acme", q, v, now, ""); len(got) != 0 {
		t.Fatalf("under a 90%% threshold: %+v", got)
	}
	v = settings.Defaults()
	v.NotifyQuota = false
	if got := quotaAlerts(7, "acme", q, v, now, ""); len(got) != 0 {
		t.Fatalf("switched off: %+v", got)
	}
}

// A balance under the threshold warns once per drop: the warning is keyed on
// the last read that saw it at or over the threshold, not on the day.
func TestBalanceAlert(t *testing.T) {
	v := settings.Defaults()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	low := provider.Quota{Kind: provider.QuotaMoney, Balance: pct(1.5), BalanceUnit: "USD"}
	got := quotaAlerts(7, "acme", low, v, now, "")
	if len(got) != 1 || got[0].Kind != store.NoteBalanceLow || !strings.Contains(got[0].Title, "1.50 USD") || got[0].DedupKey != "balance_low:7:above:never" || !got[0].Once {
		t.Fatalf("low balance = %+v", got)
	}
	again := quotaAlerts(7, "acme", low, v, now.Add(48*time.Hour), "")
	if again[0].DedupKey != got[0].DedupKey {
		t.Fatalf("two days on, still low: dedup %q, want %q", again[0].DedupKey, got[0].DedupKey)
	}
	if k := quotaAlerts(7, "acme", low, v, now, "2026-11-25T08:00:00.000Z")[0].DedupKey; k != "balance_low:7:above:2026-11-25T08:00:00.000Z" {
		t.Fatalf("after a top-up: dedup %q", k)
	}
	if got := quotaAlerts(7, "acme", provider.Quota{Kind: provider.QuotaMoney, Balance: pct(2)}, v, now, ""); len(got) != 0 {
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

// An empty prepaid balance reads 0.00, not -0.00.
func TestBalanceAlertNoNegativeZero(t *testing.T) {
	got := quotaAlerts(7, "acme", provider.Quota{Kind: provider.QuotaMoney, Balance: pct(-0.0001), BalanceUnit: "USD"}, settings.Defaults(), time.Now(), "")
	if len(got) != 1 || !strings.HasSuffix(got[0].Title, "down to 0.00 USD") {
		t.Fatalf("title = %+v", got)
	}
	if got := quotaAlerts(7, "acme", provider.Quota{Kind: provider.QuotaMoney, Balance: pct(-1.5)}, settings.Defaults(), time.Now(), ""); !strings.Contains(got[0].Title, "-1.50") {
		t.Fatalf("a real overdraft keeps its sign: %q", got[0].Title)
	}
}

// A balance that stays empty is one warning across days and hourly checks;
// after a top-up and a new drop it is said again.
func TestAlertCheckSaysLowBalanceOncePerDrop(t *testing.T) {
	var mu sync.Mutex
	body := `{"balance":0,"unit":"USD"}`
	setBody := func(b string) { mu.Lock(); body = b; mu.Unlock() }
	upstream := func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		b := body
		mu.Unlock()
		(&fakeUpstream{models: []string{"m"}, quotaBody: b}).handler()(w, r)
	}
	h := settingsHarness(t, upstream)
	day := time.Now()
	rows := func() int {
		var n int
		if err := h.db.Reader().QueryRow(`SELECT COUNT(*) FROM notifications WHERE kind='balance_low'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for i := 0; i < 3; i++ {
		h.server.checkAlerts(context.Background(), day.Add(time.Duration(i)*24*time.Hour))
	}
	if n := rows(); n != 1 {
		t.Fatalf("empty for three days: %d warnings, want 1", n)
	}
	setBody(`{"balance":10,"unit":"USD"}`)
	h.server.checkAlerts(context.Background(), day.Add(72*time.Hour))
	setBody(`{"balance":1,"unit":"USD"}`)
	h.server.checkAlerts(context.Background(), day.Add(73*time.Hour))
	h.server.checkAlerts(context.Background(), day.Add(74*time.Hour))
	if n := rows(); n != 2 {
		t.Fatalf("topped up then low again: %d warnings, want 2", n)
	}
}

// A usage page that refuses the sign-in is said once in the bell and logged
// once at Info with its status only (filtered at Warn); it is not read again
// until the provider's settings change. The provider's error body never
// reaches the log.
func TestAlertCheckStopsOnRefusedSignIn(t *testing.T) {
	for _, tc := range []struct {
		level slog.Level
		want  bool
	}{{slog.LevelInfo, true}, {slog.LevelWarn, false}} {
		var reads atomic.Int32
		upstream := func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/usage") {
				reads.Add(1)
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"error":{"message":"Your session has expired","code":"expired_session_key"}}`))
				return
			}
			(&fakeUpstream{models: []string{"m"}}).handler()(w, r)
		}
		var buf bytes.Buffer
		h := newHarness(t, upstream, func(o *Options) {
			m, err := settings.New(context.Background(), o.DB, settings.Defaults(), nil)
			if err != nil {
				t.Fatal(err)
			}
			o.Settings = m
			o.DB.SetOperatorSettings(m)
			o.Log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: tc.level}))
		})
		for i := 0; i < 3; i++ {
			h.server.checkAlerts(context.Background(), time.Now())
		}
		if n := reads.Load(); n != 1 {
			t.Fatalf("usage page read %d times, want 1", n)
		}
		var rows int
		var title string
		if err := h.db.Reader().QueryRow(`SELECT COUNT(*), COALESCE(MAX(title),'') FROM notifications WHERE kind='quota_read_refused'`).Scan(&rows, &title); err != nil {
			t.Fatal(err)
		}
		if rows != 1 || !strings.Contains(title, "sign-in was refused") {
			t.Fatalf("refused notes = %d %q, want one", rows, title)
		}
		out := buf.String()
		if got := strings.Count(out, "alert check: quota sign-in refused"); got != map[bool]int{true: 1, false: 0}[tc.want] {
			t.Fatalf("level %v: logged %d times\n%s", tc.level, got, out)
		}
		if tc.want && !strings.Contains(out, "status=401") {
			t.Fatalf("no status in the log:\n%s", out)
		}
		if strings.Contains(out, "expired_session_key") || strings.Contains(out, "quota read failed") {
			t.Fatalf("the provider's body reached the log:\n%s", out)
		}
		// A pasted cookie (any edit to the provider) earns one fresh read.
		if _, err := h.db.Writer().Exec(`UPDATE accounts SET updated_at='2099-01-01T00:00:00.000Z'`); err != nil {
			t.Fatal(err)
		}
		h.server.checkAlerts(context.Background(), time.Now())
		h.server.checkAlerts(context.Background(), time.Now())
		if n := reads.Load(); n != 2 {
			t.Fatalf("after an edit: usage page read %d times, want 2", n)
		}
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

// Retention is off by default; with a number of days it rolls older calls up,
// logged at Info (filtered at Warn), and a failure is a Warn.
func TestRetentionRunsOnlyWhenSet(t *testing.T) {
	oldCall := func(h *harness) {
		t.Helper()
		ts := time.Now().AddDate(0, 0, -60).UTC().Format("2006-01-02T15:04:05.000Z")
		if _, err := h.db.Writer().Exec(`INSERT INTO calls (ts, account, surface, model, status) VALUES (?, 'up', 'chat', 'm', 200)`, ts); err != nil {
			t.Fatal(err)
		}
	}
	withRetention := func(buf *bytes.Buffer, level slog.Level, days int) *harness {
		return newHarness(t, func(http.ResponseWriter, *http.Request) {}, func(o *Options) {
			m, err := settings.New(context.Background(), o.DB, settings.Defaults(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if days > 0 {
				if _, err := m.Patch(context.Background(), 0, map[string]json.RawMessage{"ledger_retention_days": json.RawMessage(strconv.Itoa(days))}); err != nil {
					t.Fatal(err)
				}
			}
			o.Settings = m
			o.Log = slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: level}))
		})
	}
	var buf bytes.Buffer
	h := withRetention(&buf, slog.LevelInfo, 0)
	oldCall(h)
	h.server.applyRetention(context.Background(), time.Now())
	if n := countTable(t, h, "calls"); n != 1 {
		t.Fatalf("retention off rolled calls up: %d left", n)
	}
	for _, tc := range []struct {
		level slog.Level
		want  bool
	}{{slog.LevelInfo, true}, {slog.LevelWarn, false}} {
		var buf bytes.Buffer
		h := withRetention(&buf, tc.level, 40)
		oldCall(h)
		h.server.applyRetention(context.Background(), time.Now())
		if n := countTable(t, h, "calls"); n != 0 {
			t.Fatalf("a 60-day-old call with 40 days kept: %d left", n)
		}
		if got := strings.Contains(buf.String(), "ledger rolled up"); got != tc.want {
			t.Fatalf("level %v: logged=%v\n%s", tc.level, got, buf.String())
		}
	}
	buf.Reset()
	h = withRetention(&buf, slog.LevelWarn, 40)
	_ = h.db.Close()
	h.server.applyRetention(context.Background(), time.Now())
	if !strings.Contains(buf.String(), "ledger retention failed") {
		t.Fatalf("a closed database was not logged:\n%s", buf.String())
	}
}

// Retention below a month would cut into budget windows, so it is refused.
func TestRetentionSettingRange(t *testing.T) {
	for _, days := range []int{1, 31, 3651} {
		v := settings.Defaults()
		v.LedgerRetentionDays = days
		if v.Validate() == nil {
			t.Errorf("%d days accepted", days)
		}
	}
	for _, days := range []int{0, 32, 365} {
		v := settings.Defaults()
		v.LedgerRetentionDays = days
		if err := v.Validate(); err != nil {
			t.Errorf("%d days refused: %v", days, err)
		}
	}
}

// Any successful quota read (the Quota button, a key test) lifts a refusal,
// so the hourly check reads that provider again.
func TestQuotaReadLiftsRefusal(t *testing.T) {
	h := settingsHarness(t, (&fakeUpstream{models: []string{"m"}}).handler())
	h.server.quotaRefused.Store(int64(7), "mark")
	h.server.alertQuota(7, "acme", provider.Quota{Kind: provider.QuotaNone})
	if _, still := h.server.quotaRefused.Load(int64(7)); still {
		t.Fatal("a good read left the provider refused")
	}
}
