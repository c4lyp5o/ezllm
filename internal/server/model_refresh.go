package server

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/c4lyp5o/ezllm/internal/store"
)

// StartModelRefresh re-lists every enabled account's catalog at boot and on
// each interval (model_refresh.interval, default 12h). A saved catalog
// rebuilds the auto_free combos in the same transaction (store.UpsertModels),
// so models a provider adds or drops reach the free pools on their own; new
// free models are then probed (probeFreeModels) so a broken one never sits in
// a pool untested. The first pass waits bootDelay: the Claude bridge and the
// main listener are not accepting yet at process start, so an immediate pass
// fails claude-sub (and the internal routes behind gemini-web) on every restart.
func (s *Server) StartModelRefresh(ctx context.Context, interval, bootDelay time.Duration) {
	if interval < time.Minute {
		s.log.Warn("model refresh disabled: interval under 1m", "interval", interval)
		return
	}
	s.goBackground(func(bg context.Context) {
		ctx, stop := either(ctx, bg)
		defer stop()
		select {
		case <-ctx.Done():
			return
		case <-time.After(bootDelay):
		}
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
	})
}

// refreshCatalogsOnce re-lists every enabled account's catalog. The account
// is read in full (store.AccountByID — custom headers, management key, usage
// convention), the same view the router and the key test use; a hand-built
// subset here once dropped custom headers from every refresh. A failed
// listing keeps the last catalog: the upstream being briefly down must not
// empty a pool.
func (s *Server) refreshCatalogsOnce(ctx context.Context) bool {
	success := true
	accounts, err := s.db.ListAccounts(ctx)
	if err != nil {
		success = false
		s.log.Warn("model refresh: list accounts failed", "err", err)
		return false
	}
	for _, summary := range accounts {
		if !summary.Enabled {
			continue
		}
		keys, err := s.db.ListKeys(ctx, summary.ID)
		if err != nil {
			success = false
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
			success = false
			s.log.Warn("model refresh: read account failed", "account", summary.Namespace, "err", err)
			continue
		}
		adapter, err := s.registry.Get(acct.Kind)
		if err != nil {
			success = false
			s.log.Warn("model refresh: adapter unavailable", "account", summary.Namespace, "err", err)
			continue
		}
		models, err := adapter.ListModels(ctx, *acct, key)
		if err != nil {
			success = false
			s.log.Warn("model refresh failed; keeping last catalog", "account", summary.Namespace, "err", err)
			s.notify(store.NotificationInput{
				Kind: store.NoteCatalogRefreshFailed, Severity: "warn",
				Title:    "Catalog refresh failed for " + summary.Namespace,
				Body:     "Routing keeps the last catalog. " + err.Error(),
				Data:     map[string]any{"account_id": summary.ID, "account": summary.Namespace},
				DedupKey: "catalog_refresh_failed:" + strconv.FormatInt(summary.ID, 10),
			})
			continue
		}
		if err := s.saveCatalog(ctx, summary.ID, summary.Namespace, models, "catalog refresh"); err != nil {
			success = false
			s.log.Warn("model refresh: persist failed", "account", summary.Namespace, "err", err)
			continue
		}
		s.log.Info("model catalog refreshed", "account", summary.Namespace, "models", len(models))
		acctID := summary.ID
		s.goBackground(func(bg context.Context) { s.probeFreeModels(bg, acctID, "catalog refresh") })
	}
	// Models that just arrived get prices now, not at the next daily run.
	// One pass covers every account (the price source is one document).
	if s.pricesWanted.CompareAndSwap(true, false) && s.priceRefreshOn.Load() {
		res, err := s.refreshPrices(ctx, "litellm", "")
		if err != nil {
			success = false
			s.log.Warn("price refresh for new models failed", "err", err)
		} else {
			s.log.Info("priced new models", "matched", res.Matched)
		}
	}
	return success
}

// probeFreeModels probes an account's free models on every surface that
// still lacks a verdict. Free models cost no credits to probe, so they are
// probed whenever a catalog lands — refresh, a new key, a manual sync —
// rather than waiting to be sampled. Proven models are skipped, so a steady
// catalog costs nothing; the probes are paced and hold the account's sweep
// slot like any other auto-probe. Off with EZLLM_COMBO_AUTOPROBE=off.
func (s *Server) probeFreeModels(bg context.Context, acctID int64, origin string) {
	if !s.automaticProbing() || s.tester == nil {
		return
	}
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	ids, err := s.db.FreeModelIDs(ctx, acctID)
	cancel()
	if err != nil {
		s.log.Warn("free-model probe: list failed", "account", acctID, "err", err)
		return
	}
	if len(ids) == 0 {
		return
	}
	brokenBefore := s.brokenModels(bg, acctID)
	s.probeAccountModels(bg, origin, acctID, ids)
	brokenAfter := s.brokenModels(bg, acctID)
	free := make(map[string]bool, len(ids))
	for _, id := range ids {
		free[id] = true
	}
	var newlyBroken []string
	for id := range brokenAfter {
		if free[id] && !brokenBefore[id] {
			newlyBroken = append(newlyBroken, id)
		}
	}
	if len(newlyBroken) == 0 {
		return
	}
	sort.Strings(newlyBroken)
	ns := strconv.FormatInt(acctID, 10)
	if acc, err := s.db.GetAccount(bg, acctID); err == nil {
		ns = acc.Namespace
	}
	n := len(newlyBroken)
	s.notify(store.NotificationInput{
		Kind: store.NoteFreeModelBroken, Severity: "warn",
		Title: fmt.Sprintf("%d free %s on %s failed every probe", n, plural(n, "model", "models"), ns),
		Body:  "Left out of the auto-free combos: " + noteList(newlyBroken),
		Data:  map[string]any{"account_id": acctID, "account": ns, "models": newlyBroken},
	})
}

// brokenModels is the set of an account's models whose probe verdict is
// "unsupported" on every surface — the ones auto-free pools leave out.
func (s *Server) brokenModels(bg context.Context, acctID int64) map[string]bool {
	ctx, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	out := map[string]bool{}
	rows, err := s.db.ListModels(ctx, acctID)
	if err != nil {
		return out
	}
	no := func(b *bool) bool { return b != nil && !*b }
	for _, m := range rows {
		if no(m.Chat) && no(m.Anthropic) && no(m.Responses) {
			out[m.ID] = true
		}
	}
	return out
}
