package main

// M5 rules-engine wiring: adapts *store.DB to the rules package's RuleSource
// and UsageSource, and connects the Engine's predicate to the router's
// Eligible seam.
//
// Kept out of main.go because it is glue with a little logic (CSV day
// parsing) — main.go stays the wiring hub, this stays the adapter.

import (
	"context"
	"strconv"
	"strings"

	"github.com/c4lyp5o/ezllm/internal/rules"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// storeAdapter exposes *store.DB as the rules package's narrow interfaces.
// The rules package deliberately does not import store (no cycle, and the
// predicate stays testable against fakes); all store knowledge lives here.
type storeAdapter struct{ db *store.DB }

// AllRules returns every enabled+disabled rule as rules.Rule values. The
// Engine filters disabled rows itself, so the adapter maps verbatim.
func (a storeAdapter) AllRules(ctx context.Context) ([]rules.Rule, error) {
	list, err := a.db.ListModelRules(ctx, 0) // accountID 0 = all
	if err != nil {
		return nil, err
	}
	out := make([]rules.Rule, 0, len(list))
	for _, m := range list {
		out = append(out, rules.Rule{
			AccountID: m.AccountID,
			ModelID:   m.ModelID,
			CapTokens: m.CapTokens,
			CapWindow: m.CapWindow,
			Window: rules.Window{
				Start: m.WinStart,
				End:   m.WinEnd,
				Days:  parseDaysCSV(m.WinDays),
				TZ:    m.WinTZ,
			},
			Enabled: m.Enabled,
		})
	}
	return out, nil
}

// UsageFor reads one metered bucket, returning just the token count the
// predicate needs. A bucket that has never been written is 0 usage (not an
// error) — store.UsageFor already returns the zero-value for that case.
func (a storeAdapter) UsageFor(ctx context.Context, accountID int64, modelID, bucket string) (int64, error) {
	c, err := a.db.UsageFor(ctx, accountID, modelID, bucket)
	if err != nil {
		return 0, err
	}
	return c.Tokens, nil
}

// emptyRuleSource reports no rules — used as the permissive fallback when the
// store-backed load fails at boot, so the gateway still routes (M2 behaviour)
// while the background ticker retries the real source.
type emptyRuleSource struct{}

func (emptyRuleSource) AllRules(context.Context) ([]rules.Rule, error) { return nil, nil }

// parseDaysCSV turns "1,2,3,4,5" into []int{1,2,3,4,5}. Empty → nil (every
// day). Entries were validated at write time by ValidateModelRule; a stray
// non-integer is skipped rather than failing the reload (a partially-parsed
// day filter is recoverable; a reload that errors would keep stale rules).
func parseDaysCSV(s string) []int {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if n, err := strconv.Atoi(part); err == nil && n >= 0 && n <= 6 {
			out = append(out, n)
		}
	}
	return out
}
