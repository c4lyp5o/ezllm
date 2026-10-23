package server

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/c4lyp5o/ezllm/internal/proxy"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// allowModel enforces the per-key model allowlist (empty = every model). It
// checks the REQUESTED id before a combo is expanded, so a key cannot reach a
// disallowed hop through an alias. On refusal it has written the 403.
func (s *Server) allowModel(w http.ResponseWriter, r *http.Request, client, requested string) bool {
	if s.db == nil {
		return true
	}
	allowed, err := s.db.ClientAllowsModel(r.Context(), client, requested)
	if err == nil && !allowed {
		writeErr(w, http.StatusForbidden, "model_not_allowed",
			fmt.Sprintf("token %q is not allowed to use model %q", client, requested))
		return false
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.Warn("client allowlist check failed", "client", client, "err", err)
	}
	return true
}

// finishDispatch is everything after the dispatcher returns, shared by the
// JSON and multipart inference paths: hop health, the client-facing status
// (writing an error body when nothing was committed), and the one ledger row.
// call arrives carrying what only the caller knows (timestamp, client, IP,
// surface, and for JSON the request shape and compression metrics); the
// served hop, status, usage and err are filled in here.
//
// The multipart path used to keep its own copy of this and drifted: a
// rate-limited combo answered 502 instead of 429, and hop health and the
// failover trail were never recorded.
func (s *Server) finishDispatch(w http.ResponseWriter, r *http.Request, call store.Call, routes []proxy.Route, served proxy.Route, res proxy.Result, ferr error) {
	if served.Alias == "" {
		// Resolution or swap failed before any hop committed: the ledger
		// still needs the REQUESTED target, or the row lands with no alias
		// and disappears from alias-grouped usage.
		served = routes[0]
	}
	// served is the route that ACTUALLY committed a response — it may be hop
	// 2 or 3 after failover. Every ledger field must describe that hop:
	// crediting routes[0] would bill a failed hop for work it did not do.
	//
	// Hop health. A bare model name is always a combo, so an alias without
	// '/' identifies one; a direct namespace/model route has no rotation to
	// inform. A hop-attributable failure releases a hold_until_failure block
	// so the next dispatch rotates; a 4xx is the REQUEST's fault and keeps
	// the hold (operator decision 2026-10-13).
	if hopAttributableFailure(res, ferr) && !strings.Contains(served.Alias, "/") {
		s.resolver.MarkHopBad(served.Alias, served)
	}
	// Only a delivered, completed 2xx moves sticky_last_good: a translated
	// stream that ended in a late error frame delivered a 200 header but did
	// NOT serve the request, and must not make a hop that cuts its answers
	// look healthy. The ledger keeps the delivered status and carries the
	// outcome in its err column.
	if ferr == nil && res.Status >= 200 && res.Status < 300 && res.StreamCompleted() && !strings.Contains(served.Alias, "/") {
		s.resolver.MarkHopGood(served.Alias, served)
	}
	// least_used counts load, not success: any hop that answered (committed
	// a response) carried this request.
	if res.Committed {
		s.resolver.NoteUse(served.Alias, served)
	}
	if info := reqInfoOf(r); info != nil {
		info.account = served.Account.Namespace
		info.model = served.Model
	}

	status := res.Status
	if ferr != nil {
		var rl *proxy.RateLimited
		if errors.As(ferr, &rl) {
			// Every candidate hop was out of its configured request budget,
			// so NO upstream call was made. That is a refusal, not a gateway
			// fault: answer 429 and name the earliest moment the request
			// could succeed, so a client can wait instead of blind-retrying.
			status = http.StatusTooManyRequests
			rule := map[string]any{
				"kind":   "rate",
				"detail": fmt.Sprintf("%d requests per %s", rl.Limit, rl.Window),
			}
			if !rl.NextAllowed.IsZero() {
				rule["next_allowed"] = rl.NextAllowed.Format(time.RFC3339)
			}
			writeErrExtra(w, status, "model_unavailable", ferr.Error(),
				map[string]any{"reason": ferr.Error(), "rule": rule})
		} else {
			// Nothing (or only headers) reached the client. Emit an explicit
			// 502 so a transport failure never looks like an empty 200. The
			// gate is Committed, NOT Status: commit can fail on the
			// non-stream body read while still carrying the upstream's 200.
			status = http.StatusBadGateway
			if !res.Committed {
				writeErr(w, status, "upstream_error", "upstream request failed: "+ferr.Error())
			}
		}
	} else if status == 0 {
		status = http.StatusBadGateway
	}

	call.Alias = served.Alias
	call.AccountID, call.Account = served.Account.ID, served.Account.Namespace
	call.KeyID, call.KeyHint, call.Model = served.KeyID, served.KeyHint, served.Model
	call.Status, call.Stream = status, res.Stream
	call.TTFTms, call.Totalms = res.TTFTms, res.Totalms
	call.EndpointID, call.UpstreamModel = res.EndpointID, res.UpstreamModel
	applyUsage(&call, res.Usage)
	if ferr != nil {
		call.Err = ferr.Error()
	} else if res.Err != "" {
		call.Err = res.Err
	}
	// Failover trail: every candidate that did NOT serve, appended to the
	// served row's err. One request stays ONE ledger row (the billing
	// truth), but the hops that "hopped" become visible in the dashboard.
	call.Err = appendNote(call.Err, failoverTrail(res.Attempts))
	// Request-wide attempt usage (addendum #3): what the budget allowed and
	// what was spent. Unlimited is a word, never a zero-left ambiguity.
	if res.UpstreamAttempts > 0 {
		budget := "unlimited"
		if res.AttemptBudget > 0 {
			budget = fmt.Sprintf("%d", res.AttemptBudget)
		}
		call.Err = appendNote(call.Err, fmt.Sprintf("upstream attempts %d/%s", res.UpstreamAttempts, budget))
	}
	call.BlockPos = served.BlockPos
	// Attribution for hops whose WIRE differs from the client's surface: the
	// bodies were translated; usage still follows the normalized cold-token
	// convention observed on the upstream wire. That covers both the
	// anthropic->openai-chat leg and a hop curated onto the Responses wire.
	call.Translated = served.Translated || served.ResponsesWire
	s.ledger(call)
}

// appendNote joins ledger err notes with "; ", skipping empty ones.
func appendNote(err, note string) string {
	switch {
	case note == "":
		return err
	case err == "":
		return note
	}
	return err + "; " + note
}
