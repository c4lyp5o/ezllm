package server

// M3 admin API: accounts, keys (with the mandatory 7-step test), models sync,
// quota, combos, tokens, export. Contract: docs/API.md.
//
// Security rules enforced here:
//   - every route needs a bearer token with the `admin` role
//   - no credential plaintext ever appears in a response (hints only)
//   - a key row is written ONLY after a passing test (422 → nothing stored)
//   - error bodies preserve the provider's own words + the step that failed

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/proxy"
	"github.com/c4lyp5o/ezllm/internal/router"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// cookieOrigins refuses cookie-authenticated writes from another origin.
// SameSite=Strict only separates sites: a page on another port of the same
// host, or a sibling subdomain, still sends the session cookie, and a
// text/plain POST needs no preflight. The browser's Sec-Fetch-Site (or Origin
// against Host) tells those apart; bearer clients and non-browser callers send
// neither and are unaffected.
var cookieOrigins = http.NewCrossOriginProtection()

// admin wraps a handler in auth + the admin role. 403 without the role, so a
// token that can only infer never sees account/key surfaces.
func (s *Server) admin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			if c, err := r.Cookie("ezllm_session"); err == nil {
				token = c.Value
				if err := cookieOrigins.Check(r); err != nil {
					writeErr(w, http.StatusForbidden, "forbidden", "cross-origin request refused")
					return
				}
			}
		}
		if s.db != nil && s.db.DashboardSession(r.Context(), token) {
			ctx := proxy.WithCaller(r.Context(), "dashboard:"+store.HashToken(token))
			ctx = context.WithValue(ctx, rolesKey{}, []string{"admin"})
			ctx = context.WithValue(ctx, clientKey{}, "dashboard")
			next(w, r.WithContext(ctx))
			return
		}
		s.authed(func(w http.ResponseWriter, r *http.Request) {
			if !hasRole(r, "admin") {
				// A client token without the admin role is a misconfiguration
				// someone will hunt for; name the path, never the token.
				if s.log.Enabled(r.Context(), slog.LevelDebug) {
					s.log.Debug("admin refused", "why", "client token lacks the admin role", "path", logRequestPath(r))
				}
				writeErr(w, http.StatusForbidden, "forbidden", "admin role required")
				return
			}
			next(w, r)
		})(w, r)
	}
}

// adminBodyMax caps an admin JSON body.
const adminBodyMax = 1 << 20

// decodeBody reads a bounded JSON body into dst. Unknown fields are rejected:
// a typo'd field name silently ignored is how a "saved" setting never saves.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	return decodeBodyLimit(w, r, dst, adminBodyMax)
}

// decodeBodyLimit shares validation and deadlines with smaller admin payloads.
func decodeBodyLimit(w http.ResponseWriter, r *http.Request, dst any, max int64) bool {
	// Lifted only after a clean read: on failure the deadline must stay, so
	// net/http's post-handler drain of the unread body fails fast too
	// (see limitBodyRead).
	lift := limitBodyRead(w, adminBodyTimeout)
	// An oversized body is refused, never truncated: the cut prefix would
	// decode as the whole body.
	body, tooLarge, err := readBody(r, max)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "could not read body: "+err.Error())
		return false
	}
	if tooLarge {
		writeErr(w, http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("request body exceeds %d bytes", max))
		return false
	}
	if !decodeJSONBytes(w, body, dst) {
		return false
	}
	lift()
	return true
}

// decodeOptionalBody is decodeBody for a route whose body may be left out.
// It reads to find out, so a body sent without Content-Length (chunked) is
// decoded too: a ContentLength > 0 guard skipped those and ran the defaults.
func decodeOptionalBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.ContentLength == 0 {
		return true
	}
	lift := limitBodyRead(w, adminBodyTimeout)
	body, tooLarge, err := readBody(r, adminBodyMax)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "could not read body: "+err.Error())
		return false
	}
	if tooLarge {
		writeErr(w, http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("request body exceeds %d bytes", adminBodyMax))
		return false
	}
	if len(bytes.TrimSpace(body)) == 0 {
		lift()
		return true
	}
	if !decodeJSONBytes(w, body, dst) {
		return false
	}
	lift()
	return true
}

// decodeJSONBytes decodes exactly one JSON object with no unknown fields.
func decodeJSONBytes(w http.ResponseWriter, body []byte, dst any) bool {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request_error",
			"invalid JSON body: "+err.Error())
		return false
	}
	// Refuse trailing data ("{}{}", "{}]"): only EOF may follow the value.
	// More() is not enough — it reports false before a closing delimiter.
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeErr(w, http.StatusBadRequest, "invalid_request_error",
			"body must contain a single JSON object")
		return false
	}
	return true
}

// pathID parses an {id} path value.
func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "id must be a positive integer")
		return 0, false
	}
	return id, true
}

// pathID2 parses two {id}/{sub} path values.
func pathID2(w http.ResponseWriter, r *http.Request, a, b string) (int64, int64, bool) {
	x, err := strconv.ParseInt(r.PathValue(a), 10, 64)
	if err != nil || x <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", a+" must be a positive integer")
		return 0, 0, false
	}
	y, err := strconv.ParseInt(r.PathValue(b), 10, 64)
	if err != nil || y <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", b+" must be a positive integer")
		return 0, 0, false
	}
	return x, y, true
}

// writeStoreErr maps store errors onto HTTP status codes.
// reloadRules republishes the rules engine after a model_rules write so the
// change takes effect on the next request. Best-effort: a failed reload keeps
// the last-good rule set (the background ticker retries), and never fails the
// admin write itself — the row is already committed.
func (s *Server) reloadRules() {
	if s.rulesEngine == nil {
		return
	}
	if err := s.rulesEngine.Reload(context.Background()); err != nil {
		s.log.Warn("rules reload after admin write failed", "err", err)
	}
}

func (s *Server) writeStoreErr(w http.ResponseWriter, err error) {
	var conflict *store.ErrConflict
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrRuleNotFound):
		writeErr(w, http.StatusNotFound, "not_found_error", err.Error())
	case errors.As(err, &conflict):
		extra := conflict.Details
		if extra == nil {
			extra = map[string]any{}
		}
		writeErrExtra(w, http.StatusConflict, "conflict", conflict.What, extra)
	default:
		// Validation problems from store-side checks read as 422, not 500:
		// the request was well-formed, its content was not acceptable.
		if strings.HasPrefix(err.Error(), "store: ") || looksLikeValidation(err) {
			writeErr(w, http.StatusUnprocessableEntity, "invalid_request_error", err.Error())
			return
		}
		s.log.Error("admin store error", "err", err)
		writeErr(w, http.StatusInternalServerError, "server_error", "internal error")
	}
}

func looksLikeValidation(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, p := range []string{"required", "must be", "must not", "invalid"} {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return false
}

// ── GET /admin/overview ─────────────────────────────────────────────────────

// overviewUsage is the overview's trailing-24h report for one grouping,
// shared across callers for reportTTL.
func (s *Server) overviewUsage(groupBy string) ([]store.UsageRow, error) {
	v, _, err := s.reports.do("overview|"+groupBy, func() (any, error) {
		ctx, cancel := context.WithTimeout(s.bg.ctx, reportTimeout)
		defer cancel()
		now := time.Now()
		return s.db.UsageReport(ctx, now.Add(-24*time.Hour), now, groupBy)
	})
	if err != nil {
		return nil, err
	}
	return v.([]store.UsageRow), nil
}

// handleOverview is the dashboard's single first-paint round trip.
func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	accounts, err := s.db.ListAccounts(ctx)
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	// The two 24h breakdowns are the overview's heavy part (window scans with
	// per-group medians); every tab polls them, so they are shared through
	// the report cache for reportTTL.
	usage, err := s.overviewUsage("account")
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	surfaceUsage, err := s.overviewUsage("surface")
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	recent, err := s.recentCalls(ctx, 20)
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	health, err := s.healthSnapshot(ctx)
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	// Live block_round_robin cursors, so the recent-calls table can show which
	// hop currently owns each combo's block. In-process state (not ledger data)
	// — nil resolver in unit tests yields an empty list.
	cursors := []router.BlockCursor{}
	if s.resolver != nil {
		cursors = s.resolver.BlockCursors()
		// After a restart (or before a combo's first dispatch) the in-process
		// map is empty, and the recent-calls table then renders a bare "▮ 1"
		// for rows whose "/N" lived only in the cursor. Seed the size from the
		// combo row so the by-name join always finds block_size; hop/remaining
		// stay empty = no live block yet. Disabled combos are skipped (the
		// resolver refuses them, so their rows are historical).
		if combos, cerr := s.db.ListCombos(ctx); cerr == nil {
			have := make(map[string]bool, len(cursors))
			for _, c := range cursors {
				have[c.Combo] = true
			}
			for _, c := range combos {
				if !c.Enabled || c.Strategy != "block_round_robin" || have[c.Name] {
					continue
				}
				size := c.BlockSize
				if size < 1 {
					size = 1
				}
				cursors = append(cursors, router.BlockCursor{Combo: c.Name, BlockSize: size, Reason: "cold start"})
			}
			sort.Slice(cursors, func(i, j int) bool { return cursors[i].Combo < cursors[j].Combo })
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"health":           health,
		"usage":            usage,
		"usage_by_surface": surfaceUsage,
		"accounts":         accounts,
		"recent_calls":     recent,
		"block_cursors":    cursors,
	})
}

// healthSnapshot counts the same rows /admin/health reports, shaped for the
// dashboard's health strip.
//
// ctx is the REQUEST's context (r.Context()), not context.Background(): six
// COUNT(*) queries issued on Background keep running after the client has
// disconnected, which is a leaked-query class — the request that wanted the
// answer is gone, so nobody is left to consume it.
//
// A count query that fails is RETURNED, not swallowed into a zero. The health
// strip is the operator's "is this thing alive" answer, so reporting
// accounts: 0 because a query errored is a lie stated in the one place where
// the truth matters most. The caller answers 503 instead.
// handleRouteExplain answers POST /admin/route-explain: a dry run of one
// routing decision. Nothing is dialed, no strategy cursor moves, no rule or
// budget counts. Body: {"model", "surface" (chat|anthropic|responses,
// default chat), "client" (a key name, optional), "est_tokens", "has_tools"}.
// The router's verdict per hop is joined with what is decided outside it:
// cooldowns, the account's context window, tool support, and the key's
// allowlist, token budget and spend cap.
func (s *Server) handleRouteExplain(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Model     string `json:"model"`
		Surface   string `json:"surface"`
		Client    string `json:"client"`
		EstTokens int64  `json:"est_tokens"`
		HasTools  bool   `json:"has_tools"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.Model) == "" {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	surface := provider.SurfaceChat
	switch in.Surface {
	case "", "chat":
	case "anthropic":
		surface = provider.SurfaceAnthropic
	case "responses":
		surface = provider.SurfaceResponses
	default:
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "surface must be chat, anthropic or responses")
		return
	}
	if s.resolver == nil {
		writeErr(w, http.StatusServiceUnavailable, "api_error", "routing is not ready")
		return
	}
	ctx := r.Context()
	ex := s.resolver.Explain(ctx, in.Model, surface)

	type hopView struct {
		router.ExplainHop
		ParkedUntil *time.Time `json:"parked_until,omitempty"`
		ParkReason  string     `json:"park_reason,omitempty"`
	}
	parks := s.dispatcher.Cooldowns()
	now := time.Now()
	hops := make([]hopView, len(ex.Hops))
	usable := 0
	for i, h := range ex.Hops {
		v := hopView{ExplainHop: h}
		if h.Status == "candidate" {
			if c, n, all := hopParks(h, parks, now); all {
				until := c.Until
				v.ParkedUntil, v.ParkReason = &until, c.Reason
				v.Status, v.Reason = "skipped", "parked: "+c.Reason
			} else if n > 0 {
				v.Reason = fmt.Sprintf("%d of %d keys parked: %s", n, h.KeyCount, c.Reason)
			}
		}
		if v.Status == "candidate" && in.EstTokens > 0 && h.MaxContextTokens > 0 && in.EstTokens > h.MaxContextTokens {
			v.Status, v.Reason = "skipped", fmt.Sprintf("context too small: %d tokens > the account's %d", in.EstTokens, h.MaxContextTokens)
		}
		if v.Status == "candidate" && in.HasTools && h.Kind == string(provider.KindGeminiWeb) {
			v.Status, v.Reason = "skipped", "gemini-web cannot take tools"
		}
		if v.Status == "candidate" {
			usable++
		}
		hops[i] = v
	}

	out := map[string]any{
		"requested": ex.Requested, "kind": ex.Kind, "strategy": ex.Strategy, "surface": string(surface),
		"hops": hops, "usable": usable, "error": ex.Error,
	}
	if ex.Kind == "combo" && ex.Strategy != "" && ex.Strategy != "failover" && usable > 1 {
		out["order_note"] = "hops are listed in the combo's order; the " + ex.Strategy + " strategy picks the order for each request"
	}
	if name := strings.TrimSpace(in.Client); name != "" {
		key := map[string]any{"name": name}
		if ok, err := s.db.ClientAllowsModel(ctx, name, ex.Requested); err != nil {
			key["error"] = "no enabled key with that name"
		} else {
			key["model_allowed"] = ok
			if b, err := s.db.ClientBudgetStatus(ctx, name); err == nil && b.Limit > 0 {
				key["budget"] = b
				key["budget_spent"] = b.Used >= b.Limit
			}
			if sp, err := s.db.ClientSpendStatus(ctx, name); err == nil && sp.Limit > 0 {
				key["spend"] = sp
				key["spend_cap_reached"] = sp.Spent >= sp.Limit
			}
		}
		out["key"] = key
	}
	writeJSON(w, http.StatusOK, out)
}

// hopParks counts the live parks on one hop's keys. A hop with several keys is
// parked only when every key is (until then its next key serves); all is true
// then, and first is the park that ends soonest, when the hop frees up.
func hopParks(h router.ExplainHop, parks []proxy.Cooldown, now time.Time) (first proxy.Cooldown, n int, all bool) {
	for _, c := range parks {
		if c.Namespace == h.Namespace && c.Model == h.Model && c.Until.After(now) {
			if n == 0 || c.Until.Before(first.Until) {
				first = c
			}
			n++
		}
	}
	return first, n, n > 0 && n >= max(h.KeyCount, 1)
}

// handleCooldowns lists the parked endpoints (GET) or ends parks early
// (DELETE): all of them, ?namespace=X for one account, or ?namespace=X&model=Y
// for one model. A model without a namespace is refused: model ids repeat
// across providers, so it would unpark more than the operator meant.
func (s *Server) handleCooldowns(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]any{"cooldowns": s.dispatcher.Cooldowns(), "now": time.Now().UTC()})
		return
	}
	q := r.URL.Query()
	ns, model := strings.TrimSpace(q.Get("namespace")), strings.TrimSpace(q.Get("model"))
	if model != "" && ns == "" {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "model needs a namespace: model ids repeat across providers")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cleared": s.dispatcher.ClearCooldowns(ns, model)})
}

func (s *Server) healthSnapshot(ctx context.Context) (map[string]any, error) {
	var accounts, keys, combos, models, calls int
	var oldest, newest string
	q := func(query string, dest *int) error {
		return s.db.Reader().QueryRowContext(ctx, query).Scan(dest)
	}
	for _, c := range []struct {
		q    string
		dest *int
	}{
		{`SELECT COUNT(*) FROM accounts`, &accounts},
		{`SELECT COUNT(*) FROM provider_keys`, &keys},
		{`SELECT COUNT(*) FROM combos`, &combos},
		{`SELECT COUNT(*) FROM models`, &models},
		{`SELECT COUNT(*) FROM calls`, &calls},
	} {
		if err := q(c.q, c.dest); err != nil {
			return nil, err
		}
	}
	if err := s.db.Reader().QueryRowContext(ctx,
		`SELECT COALESCE(MIN(ts),''),COALESCE(MAX(ts),'') FROM calls`).Scan(&oldest, &newest); err != nil {
		return nil, err
	}
	// The overview is what the DASHBOARD actually reads, and /admin/health is a
	// separate handler with its own payload — a field added to only one of them
	// is invisible to the operator (this cost a build: the card rendered nothing
	// while /admin/health answered correctly). Both surfaces carry the same
	// key; only /admin/health takes a window parameter, so the overview names
	// its fixed 24h.
	now := time.Now().UTC()
	hopStreams, err := s.db.HopStreamHealth(ctx, now.Add(-24*time.Hour), now)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"status":            "ok",
		"version":           Version,
		"schema_version":    s.db.SchemaVersion(),
		"db_path":           s.db.Path(),
		"accounts":          accounts,
		"provider_keys":     keys,
		"models":            models,
		"combos":            combos,
		"ledger_rows":       calls,
		"ledger_span":       map[string]string{"oldest": oldest, "newest": newest},
		"dropped_rows":      store.DroppedCalls(),
		"lost_rows":         store.LostRows(),
		"unmetered_rows":    store.UnmeteredRows(),
		"hop_stream_health": hopStreams,
		"stream_window_s":   24 * 3600,
		"uptime_s":          int(time.Since(s.started).Seconds()),
	}, nil
}

// callsCols is the canonical projection for every calls-ledger list view
// (the dashboard tail, the requests explorer), so both return identical JSON
// keys — tin/tout/cread/... are ledger names in the emitted row maps.
const callsCols = `ts, client, COALESCE(ip, '') AS ip, surface, alias, account, model, status, ttft_ms, total_ms,
       tokens_in, tokens_out, tokens_cached_read, tokens_cached_write, reasoning_tokens,
       COALESCE(tokens_saved, 0), COALESCE(compression_profile, ''),
       COALESCE(compression_applied, 0), COALESCE(compression_rules_fired, 0),
       COALESCE(context_tokens_pre, 0), COALESCE(context_tokens_saved, 0),
       COALESCE(compression_saved_notional, 0), COALESCE(prefix_sha, ''),
       COALESCE(session_id, ''), msg_count, tool_count, req_bytes,
       COALESCE(block_pos, 0),
       COALESCE(translated, 0),
       COALESCE(err, ''), slot_wait_ms`

// scanCalls maps rows projected with callsCols into the row shape the
// dashboard and the requests explorer share.
func scanCalls(rows *sql.Rows) ([]map[string]any, error) {
	out := []map[string]any{}
	for rows.Next() {
		var ts, client, ip, surface, alias, account, model string
		var status int
		var ttft, total, tin, tout, cread, cwrite, reasoning, slotWait any
		var saved, applied, rulesFired, contextPre, contextSaved int
		var notional, msgCount, toolCount, reqBytes, blockPos int
		var translated int
		var prefixSHA, sessionID, profile, errText string
		if err := rows.Scan(&ts, &client, &ip, &surface, &alias, &account, &model, &status,
			&ttft, &total, &tin, &tout, &cread, &cwrite, &reasoning,
			&saved, &profile, &applied, &rulesFired, &contextPre, &contextSaved,
			&notional, &prefixSHA, &sessionID, &msgCount, &toolCount, &reqBytes, &blockPos, &translated, &errText, &slotWait); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"ts": ts, "client": client, "ip": ip, "surface": surface, "alias": alias,
			"account": account, "model": model, "status": status,
			"ttft_ms": ttft, "total_ms": total,
			// subscription-bridge wait for a CLI slot, inside total_ms (v37);
			// null for other upstreams and for rows written before v37.
			"slot_wait_ms": slotWait,
			"tin":          tin, "tout": tout, "cread": cread, "cwrite": cwrite,
			"reasoning": reasoning,
			// saved = tokens that ACTUALLY shipped shorter; saved_notional =
			// what a rejected rewrite would have saved (tuning data, never in
			// the headline). empty = the request never mentioned compression;
			// "off" / "unknown-profile" / "disabled" = it asked and got no
			// change; otherwise the profile name (applied says whether bytes
			// moved).
			"saved":                saved,
			"saved_notional":       notional,
			"context_tokens_pre":   contextPre,
			"context_tokens_saved": contextSaved,
			"compression":          profile,
			"applied":              applied != 0,
			// non-zero only when caveman prose rules rewrote something
			"rules_fired": rulesFired,
			// v8 research capture (see docs/API.md — cacheability section)
			"prefix_sha": prefixSHA,
			"session_id": sessionID,
			"msg_count":  msgCount,
			"tool_count": toolCount,
			"req_bytes":  reqBytes,
			// block_round_robin: this dispatch's 1-based position in its block
			// (0 = not counted). The dashboard tags rows ▮ pos/size.
			"block_pos": blockPos,
			// translated: the hop's bodies were wire-translated (anthropic
			// client -> openai-only hop). Rows carrying it also carry the
			// INCLUSIVE tokens_in convention (see schema.sql calls.translated).
			"translated": translated != 0,
			// Verbatim err column (empty on success): carries the upstream
			// words and, for rows written after the error-cause classifier
			// shipped, the leading `[<cause>]` the dashboard renders as a chip.
			"err": errText,
		})
	}
	return out, rows.Err()
}

// queryCalls runs one filtered/ordered projection over the calls ledger.
// where/args are caller-built; orderBy must be a trusted literal (whitelisted
// by the caller — never interpolated from raw user input).
func (s *Server) queryCalls(ctx context.Context, where string, args []any, orderBy string, limit int) ([]map[string]any, error) {
	q := `SELECT ` + callsCols + ` FROM calls` + where +
		` ORDER BY ` + orderBy + ` LIMIT ?`
	rows, err := s.db.Reader().QueryContext(ctx, q, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCalls(rows)
}

// recentCalls returns the newest ledger rows for the dashboard table.
func (s *Server) recentCalls(ctx context.Context, limit int) ([]map[string]any, error) {
	return s.queryCalls(ctx, "", nil, "id DESC", limit)
}

// ── shared helpers ──────────────────────────────────────────────────────────

// accountFromSummary rebuilds the runtime Account the adapters expect. Key
// material never lives here — adapters receive it per call.
func accountFromSummary(a *store.AccountSummary) provider.Account {
	kind, err := provider.ParseKind(a.Kind)
	if err != nil {
		kind = provider.KindOpenAICompatible // validated at write time; belt+braces
	}
	// A window that will not parse degrades to "no rate limit" rather than
	// taking the account offline; the store applies the same rule on the
	// routing path, so both views agree.
	rateWindow, _ := time.ParseDuration(a.RateWindow)
	return provider.Account{
		ID:                    a.ID,
		Name:                  a.Name,
		Namespace:             a.Namespace,
		Kind:                  kind,
		BaseURL:               a.BaseURL,
		Enabled:               a.Enabled,
		RequiresSessionHeader: a.RequiresSession,
		ProbeDelay:            time.Duration(a.ProbeDelayMS) * time.Millisecond,
		QuotaMode:             a.QuotaMode,
		CapWindow:             a.CapWindow,
		CapTokens:             a.CapTokens,
		RateLimit:             a.RateLimit,
		RateWindow:            rateWindow,
		Notes:                 a.Notes,
	}
}

func protocolCounts(entries []store.ModelEntryRow) map[string]int {
	out := map[string]int{"chat": 0, "anthropic": 0, "responses": 0, "untested": 0}
	for _, e := range entries {
		if e.Chat != nil && *e.Chat {
			out["chat"]++
		}
		if e.Anthropic != nil && *e.Anthropic {
			out["anthropic"]++
		}
		if e.Responses != nil && *e.Responses {
			out["responses"]++
		}
		if e.Chat == nil && e.Anthropic == nil && e.Responses == nil {
			out["untested"]++
		}
	}
	return out
}

func parseSurfaces(names []string) []provider.Surface {
	if len(names) == 0 {
		return nil
	}
	var out []provider.Surface
	for _, n := range names {
		switch strings.ToLower(strings.TrimSpace(n)) {
		case "chat", "openai":
			out = append(out, provider.SurfaceChat)
		case "anthropic", "messages":
			out = append(out, provider.SurfaceAnthropic)
		case "responses":
			out = append(out, provider.SurfaceResponses)
		}
	}
	return out
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
