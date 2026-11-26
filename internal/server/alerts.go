package server

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/settings"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// Early warnings: a quota window nearly used, a prepaid balance running low,
// a session cookie about to expire. Each is said once per window (quota),
// once a day (balance) or once per cookie, and each has its switch and its
// threshold in Settings → Notifications.
const (
	alertFirst = 2 * time.Minute
	alertEvery = time.Hour
	// quotaKeep is how long quota reads are kept; a key's newest read always is.
	quotaKeep = 30 * 24 * time.Hour
)

// quotaAlerts turns one quota read into the warnings it calls for.
func quotaAlerts(acctID int64, ns string, q provider.Quota, v settings.Values, now time.Time) []store.NotificationInput {
	id := strconv.FormatInt(acctID, 10)
	data := map[string]any{"account_id": acctID, "account": ns}
	var out []store.NotificationInput
	if v.NotifyQuota && q.Kind == provider.QuotaPercent {
		for _, w := range []struct {
			name   string
			pct    *float64
			resets string
			period string // the window's identity when the provider gives no reset time
		}{
			{"rolling", q.PercentRolling, q.RollingResetsAt, now.UTC().Format("2006-01-02")},
			{"weekly", q.PercentWeekly, q.WeeklyResetsAt, isoWeek(now)},
			{"monthly", q.PercentMonthly, q.MonthlyResetsAt, now.UTC().Format("2006-01")},
		} {
			if w.pct == nil || *w.pct < float64(v.AlertQuotaPct) {
				continue
			}
			window := w.resets
			body := "Requests may be refused when it runs out."
			if w.resets != "" {
				body = "It resets " + w.resets + ". " + body
			} else {
				window = w.period
			}
			out = append(out, store.NotificationInput{
				Kind: store.NoteQuotaWarning, Severity: "warn",
				Title:    fmt.Sprintf("%s has used %.0f%% of its %s quota", ns, *w.pct, w.name),
				Body:     body,
				Data:     data,
				DedupKey: "quota_warning:" + id + ":" + w.name + ":" + window,
				Once:     true,
			})
		}
	}
	if v.NotifyBalance && q.Kind == provider.QuotaMoney && q.Balance != nil && *q.Balance < v.AlertBalance {
		unit := q.BalanceUnit
		if unit == "" {
			unit = "USD"
		}
		out = append(out, store.NotificationInput{
			Kind: store.NoteBalanceLow, Severity: "warn",
			Title:    fmt.Sprintf("%s balance is down to %.2f %s", ns, *q.Balance, unit),
			Body:     fmt.Sprintf("Top it up before it runs out. You are warned below %.2f.", v.AlertBalance),
			Data:     data,
			DedupKey: "balance_low:" + id + ":" + now.UTC().Format("2006-01-02"),
			Once:     true,
		})
	}
	return out
}

// cookieAlert warns when an account's session cookie expires within the
// configured hours, or has already expired.
func cookieAlert(acctID int64, ns string, expires time.Time, v settings.Values, now time.Time) (store.NotificationInput, bool) {
	left := expires.Sub(now)
	if !v.NotifyCookie || left > time.Duration(v.AlertCookieHours)*time.Hour {
		return store.NotificationInput{}, false
	}
	in := store.NotificationInput{
		Kind: store.NoteCookieExpiring, Severity: "warn",
		Title:    fmt.Sprintf("%s session cookie expires in %s", ns, roundHours(left)),
		Body:     "Paste a fresh cookie in the provider's settings to keep its quota readings.",
		Data:     map[string]any{"account_id": acctID, "account": ns, "expires_at": expires.UTC().Format(time.RFC3339)},
		DedupKey: "cookie_expiring:" + strconv.FormatInt(acctID, 10) + ":" + expires.UTC().Format(time.RFC3339),
		Once:     true,
	}
	if left <= 0 {
		in.Severity = "error"
		in.Title = ns + " session cookie has expired"
	}
	return in, true
}

func roundHours(d time.Duration) string {
	h := int(d.Round(time.Hour) / time.Hour)
	if h <= 1 {
		return "under an hour"
	}
	if h < 48 {
		return strconv.Itoa(h) + " hours"
	}
	return strconv.Itoa(h/24) + " days"
}

func isoWeek(t time.Time) string {
	y, w := t.UTC().ISOWeek()
	return fmt.Sprintf("%d-W%02d", y, w)
}

// alertQuota is called after any quota read is saved (the hourly check, the
// Quota button, a key test): the warning goes out on whichever read sees it.
func (s *Server) alertQuota(acctID int64, ns string, q provider.Quota) {
	if s.settings == nil {
		return
	}
	for _, in := range quotaAlerts(acctID, ns, q, s.settings.Current(), time.Now()) {
		s.notify(in)
	}
}

func (s *Server) startAlerts(ctx context.Context) {
	if s.db == nil || s.settings == nil {
		return
	}
	s.goBackground(func(bg context.Context) {
		ctx, stop := either(ctx, bg)
		defer stop()
		timer := time.NewTimer(alertFirst)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				s.checkAlerts(ctx, time.Now())
				s.applyRetention(ctx, time.Now())
				timer.Reset(alertEvery)
			}
		}
	})
}

// checkAlerts reads every enabled account's quota (when a quota or balance
// warning is on) and its cookie expiry, and raises what is due. A provider
// with no quota endpoint is skipped; a failed read is logged and tried again
// next hour.
func (s *Server) checkAlerts(ctx context.Context, now time.Time) {
	v := s.settings.Current()
	if _, err := s.db.PruneQuotaSnapshots(ctx, quotaKeep); err != nil {
		s.log.Warn("alert check failed", "step", "prune quota reads", "err", err)
	}
	if !v.NotifyQuota && !v.NotifyBalance && !v.NotifyCookie {
		return
	}
	accounts, err := s.db.ListAccounts(ctx)
	if err != nil {
		s.log.Warn("alert check failed", "step", "list accounts", "err", err)
		return
	}
	for _, a := range accounts {
		if !a.Enabled {
			continue
		}
		if a.ManagementKeyExpiresAt != "" {
			if exp, err := time.Parse(time.RFC3339, a.ManagementKeyExpiresAt); err == nil {
				if in, due := cookieAlert(a.ID, a.Namespace, exp, v, now); due {
					s.notify(in)
				}
			}
		}
		if v.NotifyQuota || v.NotifyBalance {
			s.readQuotaForAlerts(ctx, a)
		}
	}
}

// applyRetention rolls calls older than the retention setting into daily
// totals and deletes them. It runs with the hourly check; a run with nothing
// old enough does nothing.
func (s *Server) applyRetention(ctx context.Context, now time.Time) {
	keep := s.settings.Current().LedgerRetentionDays
	if keep == 0 {
		return
	}
	days, rows, err := s.db.RollUpLedger(ctx, now.AddDate(0, 0, -keep))
	if err != nil {
		s.log.Warn("ledger retention failed", "err", err, "days_done", days)
		return
	}
	if rows > 0 {
		s.log.Info("ledger rolled up", "days", days, "calls", rows, "keep_days", keep)
	}
}

func (s *Server) readQuotaForAlerts(ctx context.Context, a store.AccountSummary) {
	var keyID int64
	for _, k := range a.Keys {
		if k.Enabled {
			keyID = k.ID
			break
		}
	}
	if keyID == 0 {
		return
	}
	_, plaintext, err := s.db.KeyForTest(ctx, keyID)
	if err != nil {
		return
	}
	acct, err := s.db.AccountByID(ctx, a.ID)
	if err != nil {
		return
	}
	adapter, err := s.registry.Get(acct.Kind)
	if err != nil {
		return
	}
	q, err := adapter.ReadQuota(ctx, *acct, plaintext)
	if errors.Is(err, provider.ErrQuotaUnsupported) {
		return
	}
	if err != nil {
		// Info: a provider's usage page being down is routine, and the
		// check tries again next hour.
		s.log.Info("alert check: quota read failed", "account", a.Namespace, "err", err.Error())
		return
	}
	if err := s.db.InsertQuotaSnapshot(ctx, keyID, q, true, ""); err != nil {
		s.log.Warn("alert check failed", "step", "save quota read", "account", a.Namespace, "err", err)
		return
	}
	s.alertQuota(a.ID, a.Namespace, q)
}
