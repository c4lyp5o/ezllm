package server

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/c4lyp5o/ezllm/internal/store"
)

// spendCacheTTL is how long one client's spend answer is reused. Spend is
// derived from the ledger and the price table on every read, which is too much
// work for every request; the cost is that a key can overshoot its cap by what
// it spends in this window (plus requests already in flight). Documented in
// docs/API.md.
const spendCacheTTL = 15 * time.Second

type spendEntry struct {
	at    time.Time
	spend store.ClientSpend
}

// admitSpend refuses a key that has reached its USD spend cap with 429, the
// same shape as the token budget. On a store failure it answers 503 rather
// than letting a capped key through. It reports whether the request may go on.
func (s *Server) admitSpend(w http.ResponseWriter, r *http.Request, name string) bool {
	now := time.Now()
	var spend store.ClientSpend
	if e, ok := s.spendCache.Load(name); ok && now.Sub(e.(spendEntry).at) < spendCacheTTL {
		spend = e.(spendEntry).spend
	} else {
		var err error
		spend, err = s.db.ClientSpendStatus(r.Context(), name)
		if errors.Is(err, store.ErrNotFound) {
			return true
		}
		if err != nil {
			slog.Warn("client spend check failed", "client", name, "err", err)
			writeErr(w, http.StatusServiceUnavailable, "budget_unavailable", "usage meter unavailable")
			return false
		}
		s.spendCache.Store(name, spendEntry{at: now, spend: spend})
	}
	if spend.Limit <= 0 || spend.Spent < spend.Limit {
		return true
	}
	s.noteSpendCapReached(name, spend)
	writeErrExtra(w, http.StatusTooManyRequests, "rate_limited",
		fmt.Sprintf("spend cap reached ($%.2f of $%.2f this %s)", spend.Spent, spend.Limit, spend.Window),
		map[string]any{"limit_usd": spend.Limit, "spent_usd": spend.Spent, "window": spend.Window})
	return false
}

// forgetSpend drops a client's cached spend, so a changed cap applies on the
// next request instead of after spendCacheTTL.
func (s *Server) forgetSpend(name string) { s.spendCache.Delete(name) }

// noteSpendCapReached raises the "budget reached" notification and Info line
// once per client per window, on the same dedupe as the token budget.
func (s *Server) noteSpendCapReached(client string, sp store.ClientSpend) {
	key := "spend\x00" + client + "\x00" + sp.Bucket
	if _, seen := budgetNoted.LoadOrStore(key, true); seen {
		return
	}
	s.log.Info("client spend cap reached", "client", client,
		"spent_usd", sp.Spent, "limit_usd", sp.Limit, "window", sp.Window)
	s.notify(store.NotificationInput{
		Kind: store.NoteClientBudgetReached, Severity: "warn",
		Title:    fmt.Sprintf("Token %q reached its %s spend cap", client, sp.Window),
		Body:     fmt.Sprintf("$%.2f of $%.2f spent; its requests are refused with 429 until the window resets.", sp.Spent, sp.Limit),
		Data:     map[string]any{"client": client, "spent_usd": sp.Spent, "limit_usd": sp.Limit, "window": sp.Window, "bucket": sp.Bucket},
		DedupKey: "client_spend:" + client + "\x00" + sp.Bucket,
	})
}
