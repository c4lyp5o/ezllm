package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	backuplib "github.com/c4lyp5o/ezllm/internal/backup"
	"github.com/c4lyp5o/ezllm/internal/settings"
	"github.com/c4lyp5o/ezllm/internal/store"
)

var errBackupsUnavailable = errors.New("backup storage is not configured")

type jobHealth struct {
	Running     bool   `json:"running"`
	LastSuccess string `json:"last_success,omitempty"`
	LastFailure string `json:"last_failure,omitempty"`
}

func (s *Server) jobStatus() map[string]jobHealth {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	out := map[string]jobHealth{}
	for k, v := range s.jobs {
		out[k] = v
	}
	return out
}
func (s *Server) runJob(kind string, fn func(context.Context) bool) {
	s.jobsMu.Lock()
	state := s.jobs[kind]
	if state.Running {
		s.jobsMu.Unlock()
		return
	}
	state.Running = true
	s.jobs[kind] = state
	s.jobsMu.Unlock()
	started := s.goBackground(func(ctx context.Context) {
		ok := fn(ctx)
		s.jobsMu.Lock()
		defer s.jobsMu.Unlock()
		state := s.jobs[kind]
		state.Running = false
		now := time.Now().UTC().Format(time.RFC3339)
		if ok {
			state.LastSuccess = now
		} else {
			state.LastFailure = now
		}
		s.jobs[kind] = state
		if s.db != nil {
			persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			defer cancel()
			_ = s.db.SetJobHealth(persistCtx, kind, store.JobHealth{LastSuccess: state.LastSuccess, LastFailure: state.LastFailure})
		}
	})
	if !started {
		s.jobsMu.Lock()
		state := s.jobs[kind]
		state.Running = false
		s.jobs[kind] = state
		s.jobsMu.Unlock()
	}

}
func (s *Server) handleBackups(w http.ResponseWriter, r *http.Request) {
	if s.backups == nil {
		writeJSON(w, 200, map[string]any{"ready": false, "running": false, "backups": []any{}})
		return
	}
	writeJSON(w, 200, s.backups.Status())
}
func (s *Server) startBackup() error {
	if s.backups == nil {
		return errBackupsUnavailable
	}
	if err := s.backups.Reserve(); err != nil {
		return err
	}
	keep := s.settings.Current().BackupKeep
	started := s.goBackground(func(ctx context.Context) {
		if err := s.backups.RunReserved(ctx, keep); err != nil {
			s.notify(store.NotificationInput{Kind: "backup_failed", Severity: "error", Title: "Backup failed", Body: "The latest backup did not complete. Earlier verified backups are retained.", DedupKey: "backup_failed"})
		}
	})
	if !started {
		s.backups.CancelReserved()
		return errors.New("server is shutting down")
	}
	return nil
}
func (s *Server) handleRunBackup(w http.ResponseWriter, r *http.Request) {
	if err := s.startBackup(); err != nil {
		writeErr(w, 409, "invalid_request_error", err.Error())
		return
	}
	writeJSON(w, 202, map[string]string{"status": "running"})
}

// One scheduler owns all timers. Changes reschedule future work without
// cancelling already-running jobs; per-job guards prevent overlap.
func (s *Server) StartSettingsJobs(ctx context.Context) {
	if s.settings == nil {
		return
	}
	if rows, err := s.db.JobHealth(ctx); err == nil {
		s.jobsMu.Lock()
		for k, h := range rows {
			s.jobs[k] = jobHealth{LastSuccess: h.LastSuccess, LastFailure: h.LastFailure}
		}
		s.jobsMu.Unlock()
	}
	s.priceRefreshOn.Store(s.settings.Current().PriceRefresh)
	s.goBackground(func(bg context.Context) {
		ctx, stop := either(ctx, bg)
		defer stop()
		previous := s.settings.Current()
		catalog := time.Now().Add(30 * time.Second)
		price := time.Now().Add(45 * time.Second)
		backup := nextDaily(previous.BackupHour, previous.Location())
		for {
			p := s.settings.Current()
			now := time.Now()
			if p.CatalogInterval != previous.CatalogInterval {
				catalog = now.Add(settings.Duration(p.CatalogInterval))
			}
			if p.PriceRefresh != previous.PriceRefresh || p.UsageTZ != previous.UsageTZ {
				price = nextDaily(3, p.Location())
			}
			if p.BackupEnabled != previous.BackupEnabled || p.BackupHour != previous.BackupHour || p.UsageTZ != previous.UsageTZ {
				backup = nextDaily(p.BackupHour, p.Location())
			}
			previous = p
			next := catalog
			if p.PriceRefresh && price.Before(next) {
				next = price
			}
			if p.BackupEnabled && backup.Before(next) {
				next = backup
			}
			timer := time.NewTimer(time.Until(next))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-s.settingsWake:
				timer.Stop()
				continue
			case <-timer.C:
			}
			p = s.settings.Current()
			now = time.Now()
			if !catalog.After(now) {
				s.runJob("catalog", func(ctx context.Context) bool { return s.refreshCatalogsOnce(ctx) })
				catalog = now.Add(settings.Duration(p.CatalogInterval))
			}
			if p.PriceRefresh && !price.After(now) {
				s.runJob("prices", func(ctx context.Context) bool {
					_, err := s.refreshPrices(ctx, "litellm", "")
					if err != nil {
						s.notify(store.NotificationInput{Kind: store.NotePriceRefreshFailed, Severity: "warn", Title: "Price refresh failed", Body: "Previous model prices are retained.", DedupKey: "price_refresh_failed"})
					}
					return err == nil
				})
				price = nextDaily(3, p.Location())
			}
			if p.BackupEnabled && !backup.After(now) {
				if err := s.startBackup(); err != nil && !errors.Is(err, backuplib.ErrBusy) {
					s.notify(store.NotificationInput{Kind: "backup_failed", Severity: "error", Title: "Scheduled backup could not start", Body: "Check backup storage and deployment configuration. Earlier verified backups are retained.", DedupKey: "backup_failed"})
				}
				backup = nextDaily(p.BackupHour, p.Location())
			}
		}
	})
}
