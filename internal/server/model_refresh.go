package server

import (
	"context"
	"time"
)

// StartModelRefresh re-lists every enabled account's catalog at boot and on
// each interval (model_refresh.interval, default 12h). A saved catalog
// rebuilds the auto_free combos in the same transaction (store.UpsertModels),
// so models a provider adds or drops reach the free pools on their own; new
// free models are then probed (probeFreeModels) so a broken one never sits in
// a pool untested.
func (s *Server) StartModelRefresh(ctx context.Context, interval time.Duration) {
	if interval < time.Minute {
		s.log.Warn("model refresh disabled: interval under 1m", "interval", interval)
		return
	}
	go func() {
		s.refreshCatalogsOnce(ctx)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.refreshCatalogsOnce(ctx)
			}
		}
	}()
}

// refreshCatalogsOnce re-lists every enabled account's catalog. The account
// is read in full (store.AccountByID — custom headers, management key, usage
// convention), the same view the router and the key test use; a hand-built
// subset here once dropped custom headers from every refresh. A failed
// listing keeps the last catalog: the upstream being briefly down must not
// empty a pool.
func (s *Server) refreshCatalogsOnce(ctx context.Context) {
	accounts, err := s.db.ListAccounts(ctx)
	if err != nil {
		s.log.Warn("model refresh: list accounts failed", "err", err)
		return
	}
	for _, summary := range accounts {
		if !summary.Enabled {
			continue
		}
		keys, err := s.db.ListKeys(ctx, summary.ID)
		if err != nil {
			s.log.Warn("model refresh: list keys failed", "account", summary.Namespace, "err", err)
			continue
		}
		var key string
		for _, k := range keys {
			if k.Enabled {
				_, key, err = s.db.KeyForTest(ctx, k.ID)
				if err == nil {
					break
				}
			}
		}
		if key == "" {
			continue
		}
		acct, err := s.db.AccountByID(ctx, summary.ID)
		if err != nil {
			s.log.Warn("model refresh: read account failed", "account", summary.Namespace, "err", err)
			continue
		}
		adapter, err := s.registry.Get(acct.Kind)
		if err != nil {
			s.log.Warn("model refresh: adapter unavailable", "account", summary.Namespace, "err", err)
			continue
		}
		models, err := adapter.ListModels(ctx, *acct, key)
		if err != nil {
			s.log.Warn("model refresh failed; keeping last catalog", "account", summary.Namespace, "err", err)
			continue
		}
		if err := s.db.UpsertModels(ctx, summary.ID, models); err != nil {
			s.log.Warn("model refresh: persist failed", "account", summary.Namespace, "err", err)
			continue
		}
		s.log.Info("model catalog refreshed", "account", summary.Namespace, "models", len(models))
		go s.probeFreeModels(summary.ID, "catalog refresh")
	}
}

// probeFreeModels probes an account's free models on every surface that
// still lacks a verdict. Free models cost no credits to probe, so they are
// probed whenever a catalog lands — refresh, a new key, a manual sync —
// rather than waiting to be sampled. Proven models are skipped, so a steady
// catalog costs nothing; the probes are paced and hold the account's sweep
// slot like any other auto-probe. Off with EZLLM_COMBO_AUTOPROBE=off.
func (s *Server) probeFreeModels(acctID int64, origin string) {
	if !s.comboAutoProbe || s.tester == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	ids, err := s.db.FreeModelIDs(ctx, acctID)
	cancel()
	if err != nil {
		s.log.Warn("free-model probe: list failed", "account", acctID, "err", err)
		return
	}
	if len(ids) == 0 {
		return
	}
	s.probeAccountModels(origin, acctID, ids)
}
