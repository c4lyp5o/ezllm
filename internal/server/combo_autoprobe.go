package server

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/registration"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// comboProbeSurfaces are the wires a hop can be dialed on; a verdict on each
// is what lets the router translate instead of sending a body the hop refuses.
var comboProbeSurfaces = []provider.Surface{
	provider.SurfaceChat, provider.SurfaceAnthropic, provider.SurfaceResponses,
}

// probeComboHops starts a background protocol probe of one combo's hops.
//
// Scope is the point: an account sweep samples a catalog that can run to 500+
// models, so a hop the operator actually routes through is often never
// sampled and stays NULL — native-only routing, which is how Claude Code met
// "Invalid input at messages.1.role" on command-code (2026-10-20). Here only
// the combo's ENABLED hop models are probed, only on the surfaces that still
// lack a verdict, and curated rows are never touched. A combo write whose
// hops are all proven costs nothing.
func (s *Server) probeComboHops(comboID int64) {
	if !s.automaticProbing() || s.tester == nil {
		return
	}
	s.goBackground(func(ctx context.Context) { s.runComboProbe(ctx, comboID) })
}

func (s *Server) runComboProbe(ctx context.Context, comboID int64) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	combos, err := s.db.ListCombos(ctx)
	if err != nil {
		s.log.Warn("combo auto-probe: list combos failed", "combo", comboID, "err", err)
		return
	}
	var combo *store.Combo
	for i := range combos {
		if combos[i].ID == comboID {
			combo = &combos[i]
			break
		}
	}
	// A disabled combo serves nothing; enabling it is a PATCH, which probes.
	if combo == nil || !combo.Enabled {
		return
	}
	byAcct := map[int64][]string{}
	type hop struct {
		acct  int64
		model string
	}
	seen := map[hop]bool{}
	for _, h := range combo.Hops {
		if h.Enabled != nil && !*h.Enabled {
			continue
		}
		k := hop{h.AccountID, h.ModelID}
		if seen[k] {
			continue
		}
		seen[k] = true
		byAcct[h.AccountID] = append(byAcct[h.AccountID], h.ModelID)
	}
	for acctID, models := range byAcct {
		s.goBackground(func(ctx context.Context) {
			s.probeAccountModels(ctx, "combo "+strconv.FormatInt(comboID, 10), acctID, models)
		})
	}
}

// probeAccountModels probes the named models of one account on the surfaces
// each still lacks. It holds the account's sweep slot, so it never overlaps a
// manual sweep (and shows up in GET /admin/accounts/{id}/sync while running).
// origin names the trigger in the log ("combo 7", "catalog refresh"). Fresh
// verdicts resync the auto_free combos, so a model proven broken on every
// surface leaves the free pools at once.
func (s *Server) probeAccountModels(bg context.Context, origin string, acctID int64, models []string) {
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	acc, err := s.db.GetAccount(ctx, acctID)
	if err != nil || !acc.Enabled {
		return
	}
	entries, err := s.db.ListModels(ctx, acctID)
	if err != nil {
		s.log.Warn("auto-probe: list models failed", "origin", origin, "account", acctID, "err", err)
		return
	}
	byID := make(map[string]store.ModelEntryRow, len(entries))
	for _, e := range entries {
		byID[e.ID] = e
	}
	// Group by missing-surface set: one model untested only on anthropic must
	// not re-spend its proven openai verdict.
	groups := map[string][]provider.ModelInfo{}
	wires := map[string][]provider.Surface{}
	total := 0
	for _, m := range models {
		var missing []provider.Surface
		e, ok := byID[m]
		switch {
		case !ok:
			missing = comboProbeSurfaces
		case e.Source == "curated":
			continue // the operator's fact; the sweep would skip the write anyway
		default:
			for _, sf := range comboProbeSurfaces {
				if needsProbe(e, []provider.Surface{sf}) {
					missing = append(missing, sf)
				}
			}
		}
		if len(missing) == 0 {
			continue
		}
		names := make([]string, len(missing))
		for i, sf := range missing {
			names[i] = string(sf)
		}
		k := strings.Join(names, ",")
		groups[k] = append(groups[k], provider.ModelInfo{ID: m})
		wires[k] = missing
		total += len(missing)
	}
	if total == 0 {
		return
	}
	key, _, err := s.anyKey(ctx, acctID)
	if err != nil {
		s.log.Info("auto-probe skipped: no enabled key", "origin", origin, "account", acctID)
		return
	}
	acct := accountFromSummary(acc)

	// Same per-probe estimate as the test-all sweep, floored so a slow edge
	// still finishes and capped so a wedged one cannot hold the slot.
	budget := (acct.ProbeDelay + 1500*time.Millisecond) * time.Duration(total)
	budget = min(max(budget, 2*time.Minute), 15*time.Minute)
	surfaceNames := make([]string, 0, len(comboProbeSurfaces))
	for _, sf := range comboProbeSurfaces {
		surfaceNames = append(surfaceNames, string(sf))
	}
	run, err := s.beginSweep(acctID, surfaceNames, total, budget)
	if err != nil {
		// A running sweep owns the account; the next trigger retries.
		s.log.Info("auto-probe skipped: sweep already running", "origin", origin, "account", acctID)
		return
	}
	runCtx, runCancel := context.WithTimeout(bg, budget)
	s.attachCancel(acctID, run, runCancel)
	defer s.endSweep(acctID, run)
	defer runCancel()

	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	persisted := false
	s.log.Info("auto-probe", "origin", origin, "account", acct.Namespace, "probes", total)
	for _, k := range keys {
		res := s.tester.ProtocolSweep(runCtx, acct, key, groups[k], registration.Options{
			Surfaces: wires[k],
			ProbeAll: true,
		})
		if runCtx.Err() != nil {
			// Verdicts were written per model as the probes ran; still resync
			// so the ones that landed take effect.
			persisted = true
			break
		}
		if res != nil && len(res.Rows) > 0 {
			pctx, pcancel := context.WithTimeout(bg, 30*time.Second)
			if err := s.db.SetModelProtos(pctx, acctID, res.Rows); err != nil {
				s.log.Error("auto-probe persist failed", "origin", origin, "account", acctID, "err", err)
			} else {
				persisted = true
			}
			pcancel()
		}
	}
	if persisted {
		sctx, scancel := context.WithTimeout(bg, 30*time.Second)
		if err := s.db.SyncAutoFree(sctx); err != nil {
			s.log.Warn("auto-probe: auto-free resync failed", "origin", origin, "err", err)
		}
		scancel()
	}
}
