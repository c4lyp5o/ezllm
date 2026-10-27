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
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/proxy"
	"github.com/c4lyp5o/ezllm/internal/registration"
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
			ctx := context.WithValue(r.Context(), rolesKey{}, []string{"admin"})
			ctx = context.WithValue(ctx, clientKey{}, "dashboard")
			next(w, r.WithContext(ctx))
			return
		}
		s.authed(func(w http.ResponseWriter, r *http.Request) {
			if !hasRole(r, "admin") {
				// A client token without the admin role is a misconfiguration
				// someone will hunt for; name the path, never the token.
				s.log.Debug("admin refused", "why", "client token lacks the admin role",
					"path", r.URL.Path)
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
	// Lifted only after a clean read: on failure the deadline must stay, so
	// net/http's post-handler drain of the unread body fails fast too
	// (see limitBodyRead).
	lift := limitBodyRead(w, adminBodyTimeout)
	// An oversized body is refused, never truncated: the cut prefix would
	// decode as the whole body.
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
	lift()
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
	return map[string]any{
		"status":         "ok",
		"version":        Version,
		"schema_version": s.db.SchemaVersion(),
		"db_path":        s.db.Path(),
		"accounts":       accounts,
		"provider_keys":  keys,
		"models":         models,
		"combos":         combos,
		"ledger_rows":    calls,
		"ledger_span":    map[string]string{"oldest": oldest, "newest": newest},
		"dropped_rows":   store.DroppedCalls(),
		"lost_rows":      store.LostRows(),
		"unmetered_rows": store.UnmeteredRows(),
		"uptime_s":       int(time.Since(s.started).Seconds()),
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
       COALESCE(err, '')`

// scanCalls maps rows projected with callsCols into the row shape the
// dashboard and the requests explorer share.
func scanCalls(rows *sql.Rows) ([]map[string]any, error) {
	out := []map[string]any{}
	for rows.Next() {
		var ts, client, ip, surface, alias, account, model string
		var status int
		var ttft, total, tin, tout, cread, cwrite, reasoning any
		var saved, applied, rulesFired, contextPre, contextSaved int
		var notional, msgCount, toolCount, reqBytes, blockPos int
		var translated int
		var prefixSHA, sessionID, profile, errText string
		if err := rows.Scan(&ts, &client, &ip, &surface, &alias, &account, &model, &status,
			&ttft, &total, &tin, &tout, &cread, &cwrite, &reasoning,
			&saved, &profile, &applied, &rulesFired, &contextPre, &contextSaved,
			&notional, &prefixSHA, &sessionID, &msgCount, &toolCount, &reqBytes, &blockPos, &translated, &errText); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"ts": ts, "client": client, "ip": ip, "surface": surface, "alias": alias,
			"account": account, "model": model, "status": status,
			"ttft_ms": ttft, "total_ms": total,
			"tin": tin, "tout": tout, "cread": cread, "cwrite": cwrite,
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

// ── /admin/accounts ─────────────────────────────────────────────────────────

func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		accounts, err := s.db.ListAccounts(r.Context())
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		if accounts == nil {
			accounts = []store.AccountSummary{}
		}
		writeJSON(w, http.StatusOK, accounts)
	case http.MethodPost:
		var in store.AccountInput
		if !decodeBody(w, r, &in) {
			return
		}
		id, err := s.db.CreateAccount(r.Context(), in)
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		acc, err := s.db.GetAccount(r.Context(), id)
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, acc)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
	}
}

func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		acc, err := s.db.GetAccount(r.Context(), id)
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, acc)
	case http.MethodPatch:
		var in store.AccountInput
		if !decodeBody(w, r, &in) {
			return
		}
		if err := s.db.UpdateAccount(r.Context(), id, in); err != nil {
			s.writeStoreErr(w, err)
			return
		}
		acc, err := s.db.GetAccount(r.Context(), id)
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, acc)
	case http.MethodDelete:
		force := r.URL.Query().Get("force") == "true"
		if err := s.db.DeleteAccount(r.Context(), id, force); err != nil {
			s.writeStoreErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
	}
}

// ── key submission + the mandatory 7-step test ──────────────────────────────

// keyTestRequest is the POST /admin/accounts/{id}/keys body.
type keyTestRequest struct {
	Label         string `json:"label"`
	APIKey        string `json:"api_key"`
	SkipInference bool   `json:"skip_inference"`
	// ProbeMaxModels caps the deferred protocol sweep (default 6).
	ProbeMaxModels int `json:"probe_max_models"`
	// DeviceHandle is the dashboard device-flow alternative to APIKey
	// (github-copilot): the OAuth session never left the server, so the
	// caller names the pending flow instead of pasting its JSON. Refused
	// together with APIKey — one credential source per add.
	DeviceHandle string `json:"device_handle"`
	// Bridge names a running subscription bridge (claude-sub, chatgpt-sub)
	// whose shared secret the server fills in itself (bridges.go). Refused
	// together with APIKey, and unless the account points at that bridge.
	Bridge string `json:"bridge"`
}

// handleAddKey runs the key test and persists ONLY on success. A 422 stores
// nothing — not even a disabled row — so a bad key leaves no trace to manage.
func (s *Server) handleAddKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	accountID, ok := pathID(w, r)
	if !ok {
		return
	}
	acc, err := s.db.GetAccount(r.Context(), accountID)
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	var in keyTestRequest
	if !decodeBody(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.Label) == "" {
		in.Label = "default"
	}
	// Device flow resolves the credential server-side; the session JSON the
	// dashboard would otherwise be holding comes out of the registry, under
	// the handle it asked for, and only for this one request.
	if in.DeviceHandle != "" {
		if strings.TrimSpace(in.APIKey) != "" {
			writeErr(w, http.StatusBadRequest, "invalid_request_error",
				"pass either device_handle or api_key, not both")
			return
		}
		session, code, msg := s.ghSession(in.DeviceHandle)
		if code != 0 {
			writeErr(w, code, "invalid_request_error", msg)
			return
		}
		in.APIKey = session
	}
	if in.Bridge != "" {
		if strings.TrimSpace(in.APIKey) != "" || in.DeviceHandle != "" {
			writeErr(w, http.StatusBadRequest, "invalid_request_error",
				"pass bridge alone — the server supplies its key")
			return
		}
		token, code, msg := s.bridgeToken(in.Bridge, acc.Kind, acc.BaseURL)
		if code != 0 {
			writeErr(w, code, "invalid_request_error", msg)
			return
		}
		in.APIKey = token
	}

	sink := s.keyTestSink(w, r)
	test := s.runTest(r.Context(), acc, in, true, sink.step)
	if !test.OK {
		s.explainBridgeFailure(acc.BaseURL, test)
		sink.fail(test)
		return
	}

	// Persist after the proof: key row → test result → catalog → quota.
	acct := accountFromSummary(acc)
	keyID, hint, err := s.db.AddKey(r.Context(), accountID, in.Label, in.APIKey)
	if err != nil {
		sink.failStore(err)
		return
	}
	if err := s.persistTest(r.Context(), accountID, keyID, test, hint); err != nil {
		sink.failStore(err)
		return
	}
	s.deferProtocol(accountID, keyID, acct, in.APIKey, test, in.ProbeMaxModels)
	// Retired only now: the session has a permanent home, so the handle's
	// copy must stop working (a failed test above leaves it alive for retry).
	if in.DeviceHandle != "" {
		s.ghConsume(in.DeviceHandle)
	}

	keys, _ := s.db.ListKeys(r.Context(), accountID)
	sink.result(findKey(keys, keyID), test)
}

// handleRetest re-runs the test against an already-stored key. Unlike first
// add, a failure does NOT delete the row: it was valid once, the world may
// have changed, and losing the credential would not help anyone fix it.
func (s *Server) handleRetest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	accountID, keyID, ok := pathID2(w, r, "id", "keyId")
	if !ok {
		return
	}
	keyAccountID, pt, err := s.db.KeyForTest(r.Context(), keyID)
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	if keyAccountID != accountID {
		s.writeStoreErr(w, store.ErrNotFound)
		return
	}
	acc, err := s.db.GetAccount(r.Context(), accountID)
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	var in keyTestRequest
	if !decodeBody(w, r, &in) {
		return
	}
	in.APIKey = pt

	sink := s.keyTestSink(w, r)
	test := s.runTest(r.Context(), acc, in, true, sink.step)
	detail := test.Detail
	if !test.OK {
		// Record the failure against the live row (last_test_ok=0) but keep it.
		_ = s.db.MarkKeyTest(r.Context(), keyID, false, detail)
		s.explainBridgeFailure(acc.BaseURL, test)
		sink.fail(test)
		return
	}
	if err := s.db.MarkKeyTest(r.Context(), keyID, true, detail); err != nil {
		sink.failStore(err)
		return
	}
	acct := accountFromSummary(acc)
	if err := s.persistTest(r.Context(), accountID, keyID, test, ""); err != nil {
		sink.failStore(err)
		return
	}
	s.deferProtocol(accountID, keyID, acct, pt, test, in.ProbeMaxModels)

	keys, _ := s.db.ListKeys(r.Context(), accountID)
	sink.resultOK(findKey(keys, keyID), test)
}

// runTest executes the registration flow with sane per-request defaults.
// onStep (may be nil) receives each step as it lands — the streaming
// transport supplies one; one-shot callers pass nil and get Steps at the end.
func (s *Server) runTest(ctx context.Context, acc *store.AccountSummary, in keyTestRequest, deferProto bool, onStep func(registration.Step)) *registration.TestResult {
	opts := registration.Options{
		Label:          in.Label,
		APIKey:         in.APIKey,
		SkipInference:  in.SkipInference,
		ProbeMaxModels: in.ProbeMaxModels,
		ProtocolInline: !deferProto,
		OnStep:         onStep,
	}
	return s.tester.Run(ctx, accountFromSummary(acc), opts)
}

// persistTest stores the evidence from a passing test.
func (s *Server) persistTest(ctx context.Context, accountID, keyID int64, test *registration.TestResult, hint string) error {
	if hint == "" {
		// Retest path: refresh the stored detail only.
		return s.db.MarkKeyTest(ctx, keyID, true, test.Detail)
	}
	if err := s.db.MarkKeyTest(ctx, keyID, true, test.Detail); err != nil {
		return err
	}
	if len(test.Catalog) > 0 {
		ns := ""
		if acc, err := s.db.GetAccount(ctx, accountID); err == nil {
			ns = acc.Namespace
		}
		if err := s.saveCatalog(ctx, accountID, ns, test.Catalog, "key added"); err != nil {
			return err
		}
	}
	if test.Quota != nil {
		if err := s.db.InsertQuotaSnapshot(ctx, keyID, *test.Quota, true, ""); err != nil {
			return err
		}
	}
	return nil
}

// deferProtocol kicks off the background capability sweep once the caller has
// its 201/200 — the paced 6-model × 3-surface probe takes ~30s inline and the
// design says registration must answer in seconds.
func (s *Server) deferProtocol(accountID, keyID int64, acct provider.Account, key string, test *registration.TestResult, maxModels int) {
	if len(test.Catalog) == 0 {
		return
	}
	models := test.Catalog
	s.goBackground(func(bg context.Context) {
		// The sample sweep is capped at a few models; free models it did not
		// reach are probed after it (they cost no credits), sequenced so the
		// two never hit the account at once.
		defer s.probeFreeModels(bg, accountID, "key added")
		ctx, cancel := context.WithTimeout(bg, 10*time.Minute)
		defer cancel()
		res := s.tester.ProtocolSweep(ctx, acct, key, models, registration.Options{ProbeMaxModels: maxModels})
		if res == nil || len(res.Rows) == 0 {
			return
		}
		if err := s.db.SetModelProtos(bg, accountID, res.Rows); err != nil {
			s.log.Error("protocol sweep persist failed", "err", err, "account", acct.Namespace)
		}
		s.log.Info("protocol sweep done",
			"account", acct.Namespace, "models", res.Tested, "support", res.Support)
	})
}

// writeKeyTestFailure renders the 422 contract from docs/API.md.
func writeKeyTestFailure(w http.ResponseWriter, test *registration.TestResult) {
	status, e := keyTestError(test)
	writeJSON(w, status, map[string]any{"error": e})
}

// keyTestError computes the wire payload for a failed key test — the SAME
// bytes the streaming endpoint emits on its final line, so one source of
// truth cannot drift between transports. status is 502 when the failure is
// retryable (a 502 reads as "the provider, not the key" to proxies).
func keyTestError(test *registration.TestResult) (int, map[string]any) {
	f := test.Fail
	if f == nil {
		f = &registration.Failure{Step: "unknown", Message: "key test failed"}
	}
	status := http.StatusUnprocessableEntity
	if f.Retryable {
		status = http.StatusBadGateway
	}
	e := map[string]any{
		"message": f.Message,
		"type":    "key_test_failed",
		"code":    status,
		"step":    f.Step,
		"steps":   test.Steps,
	}
	if f.UpstreamStatus > 0 {
		e["upstream_status"] = f.UpstreamStatus
	}
	if f.UpstreamError != nil {
		e["upstream_error"] = f.UpstreamError
	}
	if f.Hint != "" {
		e["hint"] = f.Hint
	}
	if f.Retryable {
		e["retryable"] = true
	}
	return status, e
}

// ── streaming progress (docx item 8: the step chain must MOVE) ────────────

// keyTestSink receives a key test's progress and verdict. One implementor
// per transport:
//   - jsonSink: the historical one-shot response (steps arrive together)
//   - ndjsonSink: streamed — each step flushes to the browser the moment
//     it lands, so the UI colors the chain as the test walks (no fake animation)
type keyTestSink interface {
	step(registration.Step)
	// result is the add-key success (201 Created); resultOK is the retest
	// success (200 OK) — the only difference between the two verdicts.
	result(key any, test *registration.TestResult)
	resultOK(key any, test *registration.TestResult)
	fail(test *registration.TestResult)
	failStore(err error)
}

type jsonSink struct {
	w http.ResponseWriter
	s *Server
}

func (k jsonSink) step(registration.Step) {}
func (k jsonSink) result(key any, test *registration.TestResult) {
	writeJSON(k.w, http.StatusCreated, map[string]any{"key": key, "test": test})
}
func (k jsonSink) resultOK(key any, test *registration.TestResult) {
	writeJSON(k.w, http.StatusOK, map[string]any{"key": key, "test": test})
}
func (k jsonSink) fail(test *registration.TestResult) { writeKeyTestFailure(k.w, test) }
func (k jsonSink) failStore(err error)                { k.s.writeStoreErr(k.w, err) }

// ndjsonSink writes newline-delimited JSON, flushed per line:
//
//	{"type":"step","step":{"step":"catalog","ok":true,"detail":"12 models"}}
//	{"type":"step","step":{...}}
//	{"type":"error","status":422,"error":{...}}   ← same payload as a 422 body
//	{"type":"result","key":{...},"test":{...}}    ← same payload as the 201 body
//
// Headers go out before the test runs, so a failure's HTTP status cannot be
// sent afterwards; the error object therefore carries its own "status" field
// with exactly the status the one-shot path would have used.
type ndjsonSink struct {
	w  http.ResponseWriter
	fl http.Flusher
}

func (k *ndjsonSink) step(s registration.Step) {
	k.line(map[string]any{"type": "step", "step": s})
}
func (k *ndjsonSink) result(key any, test *registration.TestResult) {
	k.line(map[string]any{"type": "result", "key": key, "test": test})
}
func (k *ndjsonSink) resultOK(key any, test *registration.TestResult) {
	k.line(map[string]any{"type": "result", "key": key, "test": test})
}
func (k *ndjsonSink) fail(test *registration.TestResult) {
	status, e := keyTestError(test)
	k.line(map[string]any{"type": "error", "status": status, "error": e})
}
func (k *ndjsonSink) failStore(err error) {
	k.line(map[string]any{"type": "error", "status": http.StatusInternalServerError,
		"error": map[string]any{"message": err.Error(), "type": "store_error", "code": http.StatusInternalServerError}})
}

func (k *ndjsonSink) line(v any) {
	_ = json.NewEncoder(k.w).Encode(v)
	k.fl.Flush() // the whole point: bytes leave NOW, not at handler return
}

// keyTestSink picks the transport by the client's Accept header. The ndjson
// shape is opt-in: every existing client (and curl) keeps the one-shot JSON.
func (s *Server) keyTestSink(w http.ResponseWriter, r *http.Request) keyTestSink {
	if !strings.Contains(r.Header.Get("Accept"), "application/x-ndjson") {
		return jsonSink{w: w, s: s}
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	return &ndjsonSink{w: w, fl: fl}
}

func findKey(keys []store.KeySummary, id int64) *store.KeySummary {
	for i := range keys {
		if keys[i].ID == id {
			return &keys[i]
		}
	}
	return nil
}

// handleListKeys returns an account's keys (hints only).
func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if _, err := s.db.GetAccount(r.Context(), id); err != nil {
		s.writeStoreErr(w, err)
		return
	}
	keys, err := s.db.ListKeys(r.Context(), id)
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, keys)
}

// handleDeleteKey revokes one credential.
func (s *Server) handleDeleteKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeErr(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	accountID, keyID, ok := pathID2(w, r, "id", "keyId")
	if !ok {
		return
	}
	if err := s.db.DeleteKey(r.Context(), accountID, keyID); err != nil {
		s.writeStoreErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleToggleKey enables/disables a credential without deleting it.
func (s *Server) handleToggleKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPatch {
		writeErr(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	accountID, keyID, ok := pathID2(w, r, "id", "keyId")
	if !ok {
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.Enabled == nil {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "enabled is required")
		return
	}
	if err := s.db.EnableKey(r.Context(), accountID, keyID, *body.Enabled); err != nil {
		s.writeStoreErr(w, err)
		return
	}
	keys, _ := s.db.ListKeys(r.Context(), accountID)
	writeJSON(w, http.StatusOK, findKey(keys, keyID))
}

// ── models + sync ───────────────────────────────────────────────────────────

// handleSync runs a full catalog + protocol sweep for an account. The catalog
// refresh is inline (one request); the protocol sweep follows in the
// background, and the response reports both.

// needsProbe reports whether a model still lacks a recorded verdict on at
// least one of the surfaces about to be probed.
func needsProbe(e store.ModelEntryRow, surfaces []provider.Surface) bool {
	for _, s := range surfaces {
		var v *bool
		switch s {
		case provider.SurfaceChat:
			v = e.Chat
		case provider.SurfaceAnthropic:
			v = e.Anthropic
		case provider.SurfaceResponses:
			v = e.Responses
		}
		if v == nil {
			return true // untested on this wire: probing is information
		}
	}
	return false
}

// beginSweep registers an in-flight sweep for an account; a 409-style error
// means one is already running (the caller answers with instructions to stop
// it first — a second click must never cancel a sweep it did not start).
//
// The run carries a hard deadline (budget + grace): if its goroutine somehow
// outlives it — a wedged probe, a hung persist — the entry self-heals: a
// later beginSweep replaces it and sweepStatus reports it gone. The sweep
// must end, one way or the other, whether it succeeded or not.
func (s *Server) beginSweep(id int64, surfaces []string, probing int, budget time.Duration) (*sweepRun, error) {
	s.sweepsMu.Lock()
	defer s.sweepsMu.Unlock()
	if s.sweeps == nil {
		s.sweeps = map[int64]*sweepRun{}
	}
	if cur, running := s.sweeps[id]; running {
		if time.Now().Before(cur.deadline) {
			return nil, fmt.Errorf("sweep %d already running", id)
		}
		// Stale entry whose run outlived its deadline: replace it. The old
		// goroutine's endSweep is run-pointer-guarded, so it can no longer
		// delete this newer entry.
		if cur.cancel != nil {
			cur.cancel()
		}
	}
	run := &sweepRun{
		started:  time.Now().UTC(),
		deadline: time.Now().Add(budget + sweepDeadlineGrace),
		surfaces: surfaces,
		probing:  probing,
	}
	s.sweeps[id] = run
	return run, nil
}

// attachCancel binds the cancel func to THIS run only — the guard matters:
// without it, a slow old sweep could attach its cancel onto a newer run and
// stop work it did not start (the mirror image of the endSweep race).
func (s *Server) attachCancel(id int64, run *sweepRun, cancel context.CancelFunc) {
	s.sweepsMu.Lock()
	defer s.sweepsMu.Unlock()
	if cur, ok := s.sweeps[id]; ok && cur == run {
		cur.cancel = cancel
	}
}

// endSweep removes the entry only if it still IS this run's. An older
// goroutine finishing late must never delete a newer run's registration —
// that cross-deletion left sweeps running while the map said "idle", which
// is exactly how a later click earned a 409 against nothing visible.
func (s *Server) endSweep(id int64, run *sweepRun) {
	s.sweepsMu.Lock()
	defer s.sweepsMu.Unlock()
	if cur, ok := s.sweeps[id]; ok && cur == run {
		delete(s.sweeps, id)
	}
}

// sweepStatus reports the in-flight sweep, so a multi-minute probe is never a
// blind wait. Progress itself is the model list: probes write verdicts per
// model as they run. An entry past its deadline is reported (and dropped) as
// not running — self-healing for the operator, whatever went wrong inside.
func (s *Server) sweepStatus(id int64) map[string]any {
	s.sweepsMu.Lock()
	defer s.sweepsMu.Unlock()
	r, ok := s.sweeps[id]
	if !ok {
		return map[string]any{"running": false}
	}
	if time.Now().After(r.deadline) {
		if r.cancel != nil {
			r.cancel()
		}
		delete(s.sweeps, id)
		return map[string]any{"running": false}
	}
	return map[string]any{
		"running":    true,
		"started_at": r.started.Format(time.RFC3339),
		"surfaces":   r.surfaces,
		"probing":    r.probing,
		"elapsed_s":  int(time.Since(r.started).Seconds()),
	}
}

// handleSyncStatus: GET reports the in-flight sweep; DELETE stops it. Already
// written verdicts stay — probes are independent, and a stopped sweep is
// visible in the model list (the rows it managed to test).
func (s *Server) handleSyncStatus(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.sweepStatus(id))
	case http.MethodDelete:
		s.sweepsMu.Lock()
		run, had := s.sweeps[id]
		if had {
			delete(s.sweeps, id)
		}
		s.sweepsMu.Unlock()
		if !had {
			writeErr(w, http.StatusNotFound, "invalid_request_error", "no sweep running for this account")
			return
		}
		if run.cancel != nil {
			run.cancel()
		}
		s.log.Info("protocol sweep cancelled", "account", id)
		w.WriteHeader(http.StatusNoContent)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
	}
}

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	acc, err := s.db.GetAccount(r.Context(), id)
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	// Need a key: pick the first enabled one.
	acct := accountFromSummary(acc)
	key, keyID, err := s.anyKey(r.Context(), id)
	if err != nil {
		writeErrExtra(w, http.StatusConflict, "conflict",
			"no enabled provider key on this account — add a key first",
			map[string]any{"account_id": id})
		return
	}

	var body struct {
		Models   []string `json:"models"`
		Surfaces []string `json:"surfaces"`
		Max      int      `json:"max_models"`
		// ProbeAll is the "test all models" action: sweep the whole catalog on
		// the requested surfaces instead of sampling max_models of it.
		ProbeAll bool `json:"probe_all"`
		// OnlyUntested skips models whose requested surfaces already carry a
		// verdict (probed or curated), so a second click does not re-spend.
		// nil = true when probe_all, false otherwise.
		OnlyUntested *bool `json:"only_untested"`
	}
	if r.ContentLength > 0 {
		if !decodeBody(w, r, &body) {
			return
		}
	}
	_ = keyID

	adapter, err := s.registry.Get(acct.Kind)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	start := time.Now()
	models, err := adapter.ListModels(r.Context(), acct, key)
	if err != nil {
		writeErrExtra(w, http.StatusBadGateway, "upstream_error",
			"catalog fetch failed: "+err.Error(),
			map[string]any{"retryable": true})
		return
	}
	// Save the full listed catalog FIRST, before any scoping or early
	// return: a test-all with nothing left to probe used to answer
	// "synced: N" while keeping the old catalog.
	if err := s.saveCatalog(r.Context(), id, acct.Namespace, models, "manual sync"); err != nil {
		s.writeStoreErr(w, err)
		return
	}
	// Optional scope: the SWEEP probes only the named models. The catalog
	// was saved in full above — handing a filtered list to UpsertModels
	// would delete every unnamed row (a scoped sync once pruned command-code
	// 87→3 in prod before this was fixed).
	sweepModels := models
	if len(body.Models) > 0 {
		want := map[string]bool{}
		for _, m := range body.Models {
			want[m] = true
		}
		scoped := []provider.ModelInfo{}
		for _, m := range models {
			if want[m.ID] {
				scoped = append(scoped, m)
			}
		}
		sweepModels = scoped
	}
	probeAll := body.ProbeAll
	onlyUntested := probeAll // default for the test-all action: skip what has verdicts
	if body.OnlyUntested != nil {
		onlyUntested = *body.OnlyUntested
	}
	surfaces := parseSurfaces(body.Surfaces)
	if len(surfaces) == 0 {
		surfaces = []provider.Surface{
			provider.SurfaceChat, provider.SurfaceAnthropic, provider.SurfaceResponses,
		}
	}
	surfaceNames := make([]string, 0, len(surfaces))
	for _, s := range surfaces {
		surfaceNames = append(surfaceNames, string(s))
	}
	if onlyUntested {
		verdicts, verr := s.db.ListModels(r.Context(), id)
		if verr != nil {
			s.writeStoreErr(w, verr)
			return
		}
		byID := make(map[string]store.ModelEntryRow, len(verdicts))
		for _, e := range verdicts {
			byID[e.ID] = e
		}
		// A fresh slice, never sweepModels[:0]: without a scope sweepModels
		// IS models, and filtering in place overwrote the catalog (proven
		// models replaced by untested ones) before it was saved.
		kept := make([]provider.ModelInfo, 0, len(sweepModels))
		for _, m := range sweepModels {
			e, ok := byID[m.ID]
			if !ok {
				kept = append(kept, m) // no catalog row yet: nothing recorded, keep
				continue
			}
			// A recorded verdict on EVERY requested surface means the model has
			// been proven (or curated) on exactly the wires we are about to
			// probe — re-probing is spend, not information.
			if !needsProbe(e, surfaces) {
				continue
			}
			kept = append(kept, m)
		}
		sweepModels = kept
	}
	if probeAll && len(sweepModels) == 0 {
		entries, _ := s.db.ListModels(r.Context(), id)
		writeJSON(w, http.StatusOK, map[string]any{
			"synced":           len(models),
			"probing":          0,
			"probe_all":        true,
			"surfaces":         surfaceNames,
			"note":             "nothing to probe: every model already carries a verdict on the requested surfaces",
			"protocol_support": protocolCounts(entries),
		})
		return
	}

	// Protocol sweep: background (paced probes are slow by design).
	maxModels := body.Max
	budget := 15 * time.Minute
	if probeAll {
		// Per-probe cost ≈ pacing delay + one request. A cap of 60 minutes
		// keeps a fat catalog (408 models × 3 surfaces) inside one sitting.
		perProbe := acct.ProbeDelay + 1500*time.Millisecond
		est := perProbe * time.Duration(len(sweepModels)*len(surfaces))
		budget = 15 * time.Minute
		if est > budget {
			budget = est
		}
		if budget > 60*time.Minute {
			budget = 60 * time.Minute
		}
	}
	// One sweep per account: a new one replaces (cancels) a stale one.
	run, err := s.beginSweep(id, surfaceNames, len(sweepModels), budget)
	if err != nil {
		writeErr(w, http.StatusConflict, "conflict",
			"a protocol sweep is already running for this account — stop it first (DELETE /admin/accounts/"+fmt.Sprint(id)+"/sync)")
		return
	}
	runCtx, cancel := context.WithTimeout(s.bg.ctx, budget)
	s.attachCancel(id, run, cancel)
	s.goBackground(func(bg context.Context) {
		// Deferred first so it runs LAST, after endSweep frees the slot: free
		// models a sampled sweep skipped are probed next. A sweep the operator
		// stopped (or that ran out of budget) is not followed by more probing.
		stopped := false
		defer func() {
			if !stopped {
				s.probeFreeModels(bg, id, "manual sync")
			}
		}()
		defer s.endSweep(id, run)
		defer cancel()
		res := s.tester.ProtocolSweep(runCtx, acct, key, sweepModels, registration.Options{
			Surfaces:       surfaces,
			ProbeMaxModels: maxModels,
			ProbeAll:       probeAll,
		})
		// A cancelled or timed-out sweep has nothing to batch: every verdict it
		// managed to record was written per model as it ran, and a final write
		// on a dead context only produces a misleading "persist failed" line.
		if runCtx.Err() != nil {
			stopped = true
			return
		}
		if res != nil && len(res.Rows) > 0 {
			// Persist on a FRESH context: runCtx may already be at its
			// deadline when the last probe returns, and losing the verdicts of
			// a 20-minute sweep to an expired context would be absurd. The
			// probes already wrote per-model rows as they ran; this batch is
			// the same values, and it is what keeps a stopped sweep's work.
			pctx, pcancel := context.WithTimeout(bg, 30*time.Second)
			if err := s.db.SetModelProtos(pctx, id, res.Rows); err != nil {
				s.log.Error("sync protocol sweep persist failed", "err", err)
			}
			pcancel()
		}
	})

	entries, _ := s.db.ListModels(r.Context(), id)
	support := protocolCounts(entries)
	writeJSON(w, http.StatusOK, map[string]any{
		"synced":           len(models),
		"probing":          len(sweepModels),
		"probe_all":        probeAll,
		"surfaces":         surfaceNames,
		"budget_s":         int(budget.Seconds()),
		"protocol_tested":  0, // background sweep reports its own counts on refresh
		"protocol_support": support,
		"ms":               int(time.Since(start).Milliseconds()),
		"deferred":         true,
	})
}

// handleListModels returns the catalog with three-valued protocol state.
func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	entries, err := s.db.ListModels(r.Context(), id)
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

// handleSetModelPin sets or clears a model's surface pin.
//
// Body: {"model": "<id>", "pin": "chat"|"anthropic"|"responses"|"", "force": bool}
// The model id travels in the body, not the path: ids can contain "/".
// pin is a RawMessage so "absent" (keep), "" (clear), and a value (set) are
// distinct. A pin on a surface the probe never confirmed is refused unless
// force is true, so an operator cannot pin a model into a 400 by mistake.
func (s *Server) handleSetModelPin(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Model string          `json:"model"`
		Pin   json.RawMessage `json:"pin"`
		Force bool            `json:"force"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	model := body.Model
	if model == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "model is required")
		return
	}
	if len(body.Pin) == 0 {
		writeErr(w, http.StatusBadRequest, "bad_request", "pin is required (\"\" clears)")
		return
	}
	var pin string
	if err := json.Unmarshal(body.Pin, &pin); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "pin must be a string")
		return
	}
	if pin == "openai" {
		pin = "chat" // legacy name of the chat wire
	}
	switch {
	case pin == "", pin == "chat", pin == "anthropic", pin == "responses", store.IsServiceSurface(pin):
	default:
		writeErr(w, http.StatusBadRequest, "bad_request",
			"pin must be chat, anthropic, responses, "+strings.Join(store.ServiceSurfaceNames, ", ")+", or empty")
		return
	}

	if pin != "" && !body.Force {
		entries, err := s.db.ListModels(r.Context(), id)
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		var found *store.ModelEntryRow
		for i := range entries {
			if entries[i].ID == model {
				found = &entries[i]
				break
			}
		}
		if found == nil {
			writeErr(w, http.StatusNotFound, "not_found", "model not in this account's catalog")
			return
		}
		confirmed := map[string]*bool{"chat": found.Chat, "anthropic": found.Anthropic, "responses": found.Responses}[pin]
		if confirmed == nil || !*confirmed {
			writeErr(w, http.StatusConflict, "unconfirmed_surface",
				"the probe has not confirmed this model on "+pin+"; pass force=true to pin anyway")
			return
		}
	}

	if err := s.db.SetModelPin(r.Context(), id, model, pin); err != nil {
		s.writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"model": model, "pin": pin, "force": body.Force})
}

// handleSetModelProtocol records operator-curated protocol verdicts.
//
// Body: {"model": "<id>", "proto_chat": true|false|null, "proto_anthropic":
// ..., "proto_responses": ..., "note": "<why>", "release": bool}
// The model id travels in the body, not the path: ids can contain "/".
// Each proto_* verdict is three-state: absent keeps the recorded value, null
// clears it to untested, true/false records the verdict. A non-release write
// requires a note — the audit trail for why the fact is curated — and
// protects the row from probe sweeps until released. release=true hands the
// row back to the sweep: verdicts stay as recorded, source and note clear.
func (s *Server) handleSetModelProtocol(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Model     string          `json:"model"`
		Chat      json.RawMessage `json:"proto_chat"`
		Anthropic json.RawMessage `json:"proto_anthropic"`
		Responses json.RawMessage `json:"proto_responses"`
		Note      string          `json:"note"`
		Release   bool            `json:"release"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.Model == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "model is required")
		return
	}
	present := 0
	for _, raw := range []json.RawMessage{body.Chat, body.Anthropic, body.Responses} {
		if len(raw) != 0 {
			present++
		}
	}
	if body.Release && present > 0 {
		writeErr(w, http.StatusBadRequest, "bad_request", "release cannot be combined with proto_* verdicts")
		return
	}
	if !body.Release && present == 0 {
		writeErr(w, http.StatusBadRequest, "bad_request", "at least one proto_* verdict is required (or release=true)")
		return
	}
	if !body.Release && strings.TrimSpace(body.Note) == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "note is required: it is the audit trail for a curated fact")
		return
	}
	edits := map[string]store.ProtoEdit{}
	for name, raw := range map[string]json.RawMessage{
		"chat": body.Chat, "anthropic": body.Anthropic, "responses": body.Responses,
	} {
		if len(raw) == 0 {
			continue
		}
		edit := store.ProtoEdit{Present: true}
		switch string(raw) {
		case "null":
		case "true":
			v := true
			edit.Value = &v
		case "false":
			v := false
			edit.Value = &v
		default:
			writeErr(w, http.StatusBadRequest, "bad_request", "proto_"+name+" must be true, false, or null")
			return
		}
		edits[name] = edit
	}
	// 404 on an unknown model — the same typo protection as the pin route.
	model := body.Model
	entries, err := s.db.ListModels(r.Context(), id)
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	found := false
	for i := range entries {
		if entries[i].ID == model {
			found = true
			break
		}
	}
	if !found {
		writeErr(w, http.StatusNotFound, "not_found", "model not in this account's catalog")
		return
	}
	if err := s.db.SetModelCurated(r.Context(), id, model, strings.TrimSpace(body.Note), edits, body.Release); err != nil {
		s.writeStoreErr(w, err)
		return
	}
	// Echo the persisted truth, not the request: absent verdicts kept their
	// old values, and release keeps values while clearing provenance.
	entries, err = s.db.ListModels(r.Context(), id)
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	for i := range entries {
		if entries[i].ID != model {
			continue
		}
		writeJSON(w, http.StatusOK, entries[i])
		return
	}
	writeErr(w, http.StatusNotFound, "not_found", "model vanished while writing")
}

// handleAccountQuota performs a live quota read + snapshot now.
func (s *Server) handleAccountQuota(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	acc, err := s.db.AccountByID(r.Context(), id)
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	key, keyID, err := s.anyKey(r.Context(), id)
	if err != nil {
		writeErrExtra(w, http.StatusConflict, "conflict",
			"no enabled provider key on this account", map[string]any{"account_id": id})
		return
	}
	adapter, err := s.registry.Get(acc.Kind)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	q, err := adapter.ReadQuota(r.Context(), *acc, key)
	if err != nil && !errors.Is(err, provider.ErrQuotaUnsupported) {
		writeErrExtra(w, http.StatusBadGateway, "upstream_error",
			"quota read failed: "+err.Error(), map[string]any{"retryable": true})
		return
	}
	if q.Kind == "" {
		q.Kind = provider.QuotaNone
	}
	if err := s.db.InsertQuotaSnapshot(r.Context(), keyID, q, err == nil, errText(err)); err != nil {
		s.writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"kind": q.Kind, "raw": q.Raw, "read_at": time.Now().UTC().Format(time.RFC3339),
	})
}

// anyKey returns the first enabled key's plaintext + id.
func (s *Server) anyKey(ctx context.Context, accountID int64) (string, int64, error) {
	keys, err := s.db.ListKeys(ctx, accountID)
	if err != nil {
		return "", 0, err
	}
	for _, k := range keys {
		if k.Enabled {
			accID, pt, err := s.db.KeyForTest(ctx, k.ID)
			if err != nil || accID != accountID {
				continue
			}
			return pt, k.ID, nil
		}
	}
	return "", 0, store.ErrNotFound
}

// ── combos ──────────────────────────────────────────────────────────────────
func (s *Server) handleCombos(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		combos, err := s.db.ListCombos(r.Context())
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, combos)
	case http.MethodPost:
		// Decoded in two steps so an OMITTED `enabled` means the schema
		// default (enabled=1), not Go's false zero-value. The dashboard always
		// sends the field, but an API caller that does not gets a combo that
		// reads back fine in GET /admin/combos and then fails every inference
		// request with "combo is disabled" — a trap with no visible symptom
		// until traffic arrives. An explicit "enabled": false still disables.
		var raw struct {
			store.Combo
			Enabled *bool           `json:"enabled"`
			Profile json.RawMessage `json:"compression_profile_id"`
		}
		if !decodeBody(w, r, &raw) {
			return
		}
		c := raw.Combo
		if raw.Profile != nil {
			if string(raw.Profile) == "null" {
				c.CompressionProfileID = nil
			} else if err := json.Unmarshal(raw.Profile, &c.CompressionProfileID); err != nil {
				writeErr(w, http.StatusBadRequest, "invalid_request_error", "compression_profile_id must be an integer or null")
				return
			}
		}
		c.Enabled = raw.Enabled == nil || *raw.Enabled
		id, err := s.db.UpsertCombo(r.Context(), c)
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		s.probeComboHops(id)
		combos, _ := s.db.ListCombos(r.Context())
		for _, x := range combos {
			if x.ID == id {
				writeJSON(w, http.StatusCreated, x)
				return
			}
		}
		writeJSON(w, http.StatusCreated, map[string]any{"id": id})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
	}
}

// ── POST /admin/combos/{id}/test ────────────────────────────────────────────

// comboTestHop is one hop's probe result. Status 0 with Err set means the hop
// could not be DIALED at all (account missing, no usable key) — distinct from
// an upstream status the request actually earned.
type comboTestHop struct {
	AccountID int64  `json:"account_id"`
	Namespace string `json:"namespace"`
	Model     string `json:"model"`
	Enabled   bool   `json:"enabled"`
	Status    int    `json:"status"`
	Ms        int64  `json:"ms"`
	Err       string `json:"err,omitempty"`
	Cause     string `json:"cause,omitempty"`
	Parked    bool   `json:"parked"`
	CooldownS int64  `json:"cooldown_s,omitempty"`
}

// captureWriter is a minimal http.ResponseWriter for the probes below: the
// response body is discarded, only the status the dispatcher earned is kept.
// (httptest would do the same thing, but a test package has no business in
// production code.)
type captureWriter struct {
	hdr    http.Header
	status int
}

func (c *captureWriter) Header() http.Header {
	if c.hdr == nil {
		c.hdr = http.Header{}
	}
	return c.hdr
}
func (c *captureWriter) WriteHeader(s int) { c.status = s }
func (c *captureWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	return len(b), nil
}

// causeFromSnippet pulls the `[<cause>]` marker the error-cause classifier
// writes into the ledger err column.
func causeFromSnippet(s string) string {
	i := strings.Index(s, "[")
	if i < 0 {
		return ""
	}
	if j := strings.Index(s[i:], "]"); j > 0 {
		return s[i+1 : i+j]
	}
	return ""
}

// handleComboTest probes EVERY hop of a combo with one tiny real request per
// hop and reports the outcome. Deliberate choices, all of them operator-facing:
//
//   - No eligibility/capability/pin filtering: showing WHY the router
//     currently skips a hop is the point of the test.
//   - No translation/responses-wire marking on the probe: it is a bare
//     <surface> request, so a protocol refusal here is genuine signal about
//     the hop's native support (the markings are applied by the router).
//   - It SPENDS: one request per hop, max_tokens 16, non-streaming.
//   - It uses the real dispatcher, so parks and cooldowns behave exactly as in
//     production — a probe that parks a hop LEAVES it parked, because it failed
//     for real. The probe itself is NOT written to the ledger.
func (s *Server) handleComboTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	combos, err := s.db.ListCombos(r.Context())
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	var combo *store.Combo
	for i := range combos {
		if combos[i].ID == id {
			combo = &combos[i]
			break
		}
	}
	if combo == nil {
		writeErr(w, http.StatusNotFound, "not_found", fmt.Sprintf("no combo %d", id))
		return
	}
	if len(combo.Hops) == 0 {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_request_error", "combo has no hops")
		return
	}
	if s.dispatcher == nil || s.resolver == nil {
		writeErr(w, http.StatusServiceUnavailable, "server_error", "dispatcher unavailable")
		return
	}

	surface := provider.SurfaceChat
	if r.ContentLength > 0 {
		var in struct {
			Surface string `json:"surface"`
		}
		// A malformed body is ignored on purpose: the surface is a UI hint,
		// not a security-relevant field, and defaulting is what the button
		// sends anyway.
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&in)
		switch in.Surface {
		case "", "chat", "openai":
			surface = provider.SurfaceChat
		case "anthropic":
			surface = provider.SurfaceAnthropic
		default:
			writeErr(w, http.StatusBadRequest, "invalid_request_error", "surface must be chat or anthropic")
			return
		}
	}

	hops := make([]comboTestHop, 0, len(combo.Hops))
	for _, h := range combo.Hops {
		hr := comboTestHop{
			AccountID: h.AccountID,
			Model:     h.ModelID,
			Enabled:   h.Enabled == nil || *h.Enabled,
		}
		rt, rerr := s.resolver.TestRoute(r.Context(), h.AccountID, h.ModelID, combo.Name, surface)
		if rerr != nil {
			hr.Err = rerr.Error()
			hops = append(hops, hr)
			continue
		}
		hr.Namespace = rt.Account.Namespace

		body := probeBody(h.ModelID)
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, probePath(surface), bytes.NewReader(body))
		if reqErr != nil {
			cancel()
			hr.Err = reqErr.Error()
			hops = append(hops, hr)
			continue
		}
		start := time.Now()
		_, res, ferr := s.dispatcher.ForwardCandidates(ctx, &captureWriter{}, req, []proxy.Route{rt}, body)
		cancel()
		hr.Ms = time.Since(start).Milliseconds()

		switch {
		case ferr != nil:
			hr.Err = ferr.Error()
			// Nothing reached the client — the same 502 the inference path
			// answers with, so a transport failure never reads as status 0.
			hr.Status = http.StatusBadGateway
		default:
			hr.Status = res.Status
			hr.Err = res.Err
			hr.Cause = causeFromSnippet(res.Err)
		}
		for _, c := range s.dispatcher.Cooldowns() {
			if c.Namespace == rt.Account.Namespace && c.Model == rt.Model && c.Until.After(time.Now()) {
				hr.Parked = true
				hr.CooldownS = int64(time.Until(c.Until).Seconds())
			}
		}
		hops = append(hops, hr)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"combo":   combo.Name,
		"surface": string(surface),
		"hops":    hops,
	})
}

// probeBody is the smallest request that proves a hop answers: one user
// message, 16 tokens out, no tools, no streaming. The SAME shape works on
// both wires (both accept model/max_tokens/messages), so the surface only
// changes the path.
func probeBody(model string) []byte {
	m, _ := json.Marshal(model) // %q is not JSON for control bytes / invalid UTF-8
	return []byte(fmt.Sprintf(`{"model":%s,"max_tokens":16,"messages":[{"role":"user","content":"ezllm combo test: reply ok"}]}`, m))
}

func probePath(surface provider.Surface) string {
	if surface == provider.SurfaceAnthropic {
		return "/v1/messages"
	}
	return "/v1/chat/completions"
}

func (s *Server) handleCombo(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		combos, err := s.db.ListCombos(r.Context())
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		for _, c := range combos {
			if c.ID == id {
				writeJSON(w, http.StatusOK, c)
				return
			}
		}
		writeErr(w, http.StatusNotFound, "not_found_error", "combo not found")
	case http.MethodPatch:
		// `enabled` is decoded separately because a bool cannot say "absent":
		// the old shape dropped the BODY's enabled on the floor (it was
		// overwritten by the current value and only `?enabled=` could change
		// it), so PATCH {"enabled":false} silently left the combo routing.
		// Pointer = tri-state: nil keeps the current value.
		var body struct {
			store.Combo
			Enabled               *bool                `json:"enabled"`
			AutoFree              *bool                `json:"auto_free"`
			AutoFreeAccounts      *[]int64             `json:"auto_free_accounts"`
			AutoFreeInclude       *[]store.FreeInclude `json:"auto_free_include"`
			CompressionProfileRaw json.RawMessage      `json:"compression_profile_id"`
			ContextSizeRaw        json.RawMessage      `json:"context_size"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		c := body.Combo
		combos, err := s.db.ListCombos(r.Context())
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		var cur *store.Combo
		for i := range combos {
			if combos[i].ID == id {
				cur = &combos[i]
				break
			}
		}
		if cur == nil {
			writeErr(w, http.StatusNotFound, "not_found_error", "combo not found")
			return
		}
		// Merge: empty fields keep current values (partial PATCH semantics).
		if c.Name == "" {
			c.Name = cur.Name
		}
		if c.Strategy == "" {
			c.Strategy = cur.Strategy
		}
		if c.StickyIdleS == 0 {
			c.StickyIdleS = cur.StickyIdleS
		}
		// Same zero-value convention as sticky_idle_s: an omitted block_size
		// keeps the stored value. 1 is the smallest meaningful block, so nothing
		// is expressible only as 0.
		if c.BlockSize == 0 {
			c.BlockSize = cur.BlockSize
		}
		if body.ContextSizeRaw == nil {
			c.ContextSize = cur.ContextSize
		}
		if body.CompressionProfileRaw == nil {
			c.CompressionProfileID = cur.CompressionProfileID
		}
		if body.CompressionProfileRaw != nil {
			if string(body.CompressionProfileRaw) == "null" {
				c.CompressionProfileID = nil
			} else if err := json.Unmarshal(body.CompressionProfileRaw, &c.CompressionProfileID); err != nil {
				writeErr(w, http.StatusBadRequest, "invalid_request_error", "compression_profile_id must be an integer or null")
				return
			}
		}
		if body.ContextSizeRaw != nil {
			if string(body.ContextSizeRaw) == "null" || json.Unmarshal(body.ContextSizeRaw, &c.ContextSize) != nil {
				writeErr(w, http.StatusBadRequest, "invalid_request_error", "context_size must be a positive integer")
				return
			}
		} else {
			c.ContextSize = cur.ContextSize
		}
		if c.Hops == nil {
			c.Hops = cur.Hops
		}
		c.AutoFree, c.AutoFreeAccounts, c.AutoFreeInclude = cur.AutoFree, cur.AutoFreeAccounts, cur.AutoFreeInclude
		if body.AutoFree != nil {
			c.AutoFree = *body.AutoFree
		}
		if body.AutoFreeAccounts != nil {
			c.AutoFreeAccounts = *body.AutoFreeAccounts
		}
		if body.AutoFreeInclude != nil {
			c.AutoFreeInclude = *body.AutoFreeInclude
		}
		c.Enabled = cur.Enabled
		if body.Enabled != nil {
			c.Enabled = *body.Enabled
		}
		if r.URL.Query().Get("enabled") != "" {
			c.Enabled = r.URL.Query().Get("enabled") == "true"
		}
		newID, err := s.db.UpsertCombo(r.Context(), c)
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		s.probeComboHops(newID)
		combos, _ = s.db.ListCombos(r.Context())
		for _, x := range combos {
			if x.ID == newID {
				writeJSON(w, http.StatusOK, x)
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": newID})
	case http.MethodDelete:
		if err := s.db.DeleteCombo(r.Context(), id); err != nil {
			s.writeStoreErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
	}
}

// handleComboHops replaces a combo's ordered hop list atomically.
func (s *Server) handleComboHops(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		writeErr(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Hops []store.ComboHop `json:"hops"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if err := s.db.ReplaceHops(r.Context(), id, body.Hops); err != nil {
		s.writeStoreErr(w, err)
		return
	}
	s.probeComboHops(id)
	combos, _ := s.db.ListCombos(r.Context())
	for _, c := range combos {
		if c.ID == id {
			writeJSON(w, http.StatusOK, c)
			return
		}
	}
	writeErr(w, http.StatusNotFound, "not_found_error", "combo not found")
}

// ── tokens ──────────────────────────────────────────────────────────────────

func (s *Server) handleTokens(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		tokens, err := s.db.ListTokens(r.Context())
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, tokens)
	case http.MethodPost:
		var in store.TokenInput
		if !decodeBody(w, r, &in) {
			return
		}
		id, plaintext, err := s.db.UpsertToken(r.Context(), in)
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		tokens, _ := s.db.ListTokens(r.Context())
		var summary *store.TokenSummary
		for i := range tokens {
			if tokens[i].ID == id {
				summary = &tokens[i]
				break
			}
		}
		// The plaintext token exists in this response and nowhere else, ever.
		writeJSON(w, http.StatusCreated, map[string]any{
			"token": summary, "plaintext": plaintext,
		})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
	}
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodPatch:
		var body struct {
			Enabled       *bool    `json:"enabled"`
			Roles         []string `json:"roles"`
			CapTokens     *int64   `json:"cap_tokens"`
			CapWindow     string   `json:"cap_window"`
			AllowedModels *string  `json:"allowed_models"`
			ExpiresAt     *string  `json:"expires_at"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		if err := s.db.UpdateToken(r.Context(), id, body.Enabled, body.Roles,
			body.CapTokens, body.CapWindow, body.AllowedModels, body.ExpiresAt); err != nil {
			s.writeStoreErr(w, err)
			return
		}
		tokens, _ := s.db.ListTokens(r.Context())
		for _, t := range tokens {
			if t.ID == id {
				writeJSON(w, http.StatusOK, t)
				return
			}
		}
		writeErr(w, http.StatusNotFound, "not_found_error", "token not found")
	case http.MethodDelete:
		if err := s.db.DeleteToken(r.Context(), id); err != nil {
			s.writeStoreErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
	}
}

// ── profiles + export + quota ───────────────────────────────────────────────

// handleProfiles lists compression profiles; POST creates one (CRUD in
// compression_admin.go).
func (s *Server) handleProfiles(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.handleProfileCreate(w, r)
		return
	}
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	profiles, err := s.db.ListCompressionProfiles(r.Context())
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, profiles)
}

// handleExport dumps the full config for disaster recovery (no credentials).
//
// The ledger is STREAMED, not accumulated. The old shape built one
// []map[string]any of every call and encoded it in a single write, so the
// handler's memory scaled with the ledger — and a disaster-recovery file that
// OOMs the box is not a recovery file. Now each row is projected and written as
// it is read, so the footprint is flat regardless of ledger size and the file
// is COMPLETE (no row cap: silently omitting history from the thing you recover
// WITH is the worst possible failure for this endpoint).
//
// Config and the first ledger row are checked before headers. Later source
// failures produce an explicit incomplete manifest; write failures leave no
// successful completion marker. The ledger stays streaming and uncapped.
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	includeLedger := r.URL.Query().Get("include_ledger") == "true"
	exp, err := s.db.ExportConfig(r.Context())
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	// Validate the cursor and first row before promising a successful download.
	var rows *sql.Rows
	var first map[string]any
	if includeLedger {
		rows, err = s.db.ExportLedgerRows(r.Context())
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		defer rows.Close()
		if rows.Next() {
			first, err = store.ScanExportRow(rows)
		} else {
			err = rows.Err()
		}
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
	}
	completion := store.ExportCompletion{Complete: true, LedgerRequested: includeLedger}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="ezllm-export-%s.json"`, time.Now().UTC().Format("20060102-150405")))
	w.WriteHeader(http.StatusOK)
	if _, err = io.WriteString(w, "{"); err != nil {
		return
	}
	obj := newJSONObject(w)
	for _, f := range []struct {
		name string
		v    any
	}{
		{"settings", exp.Settings}, {"format_version", exp.FormatVersion}, {"kind", exp.Kind}, {"database_backup_required", exp.DatabaseBackupRequired},
		{"models", exp.Models}, {"model_rules", exp.Rules}, {"model_prices", exp.Prices}, {"service_bindings", exp.Bindings},
		{"exported_at", exp.ExportedAt}, {"schema_version", exp.SchemaVersion}, {"accounts", exp.Accounts}, {"combos", exp.Combos},
		{"tokens", exp.Tokens}, {"compression_profiles", exp.Compression}, {"latest_quotas", exp.Quotas},
	} {
		if err = obj.field(f.name, f.v); err != nil {
			return
		}
	}
	flusher, _ := w.(http.Flusher)
	if includeLedger {
		if err = obj.beginField("ledger"); err != nil {
			return
		}
		if err = obj.raw("["); err != nil {
			return
		}
		writeRow := func(row map[string]any) error {
			if completion.LedgerRows > 0 {
				if _, err := io.WriteString(w, ","); err != nil {
					return err
				}
			}
			if err := json.NewEncoder(w).Encode(row); err != nil {
				return err
			}
			completion.LedgerRows++
			if flusher != nil && completion.LedgerRows%500 == 0 {
				flusher.Flush()
			}
			return nil
		}
		if first != nil {
			if err = writeRow(first); err != nil {
				return
			}
		}
		for rows.Next() {
			row, scanErr := store.ScanExportRow(rows)
			if scanErr != nil {
				err = scanErr
				break
			}
			if writeErr := writeRow(row); writeErr != nil {
				return
			}
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			s.log.Error("export ledger did not complete", "err", err, "rows", completion.LedgerRows)
			completion.Complete = false
			completion.Error = "ledger export did not complete"
		}
		if _, err = io.WriteString(w, "]"); err != nil {
			return
		}
	}
	if r.Context().Err() != nil {
		completion.Complete = false
		completion.Error = "export request cancelled"
	}
	if err = obj.field("completion", completion); err != nil {
		return
	}
	if _, err = io.WriteString(w, "}"); err != nil {
		return
	}
	if flusher != nil {
		flusher.Flush()
	}
}

// handleQuotas is the poller's read side: latest snapshot per key.
func (s *Server) handleQuotas(w http.ResponseWriter, r *http.Request) {
	var ids []int64
	if raw := r.URL.Query().Get("keys"); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			if n, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64); err == nil {
				ids = append(ids, n)
			}
		}
	}
	out, err := s.db.LatestQuotas(r.Context(), ids)
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleKeyQuota forces a live quota read for one key (POST /admin/keys/{id}/quota).
func (s *Server) handleKeyQuota(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	keyID, err := strconv.ParseInt(r.PathValue("keyId"), 10, 64)
	if err != nil || keyID <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "keyId must be a positive integer")
		return
	}
	keyAcct, plaintext, err := s.db.KeyForTest(r.Context(), keyID)
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	acc, err := s.db.GetAccount(r.Context(), keyAcct)
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	adapter, err := s.registry.Get(accountFromSummary(acc).Kind)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	q, qErr := adapter.ReadQuota(r.Context(), accountFromSummary(acc), plaintext)
	if qErr != nil && !errors.Is(qErr, provider.ErrQuotaUnsupported) {
		writeErrExtra(w, http.StatusBadGateway, "upstream_error",
			"quota read failed: "+qErr.Error(), map[string]any{"retryable": true})
		return
	}
	if q.Kind == "" {
		q.Kind = provider.QuotaNone
	}
	if err := s.db.InsertQuotaSnapshot(r.Context(), keyID, q, qErr == nil, errText(qErr)); err != nil {
		s.writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"kind": q.Kind, "raw": q.Raw, "read_at": time.Now().UTC().Format(time.RFC3339),
	})
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

// ── M5 model rules ─────────────────────────────────────────────────────────

// handleModelRules serves GET (list, ?account_id= filter) and POST (upsert).
// A POST is an upsert keyed on (account_id, model_id), so re-sending a rule
// updates it rather than duplicating — the same key the router looks up.
func (s *Server) handleModelRules(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		var accountID int64
		if v := r.URL.Query().Get("account_id"); v != "" {
			accountID, _ = strconv.ParseInt(v, 10, 64)
		}
		rules, err := s.db.ListModelRules(r.Context(), accountID)
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, rules)
	case http.MethodPost:
		// Two-step decode so an omitted `enabled` means the schema default
		// (enabled=1), not Go's false zero-value — the same trap handleCombos
		// avoids: a rule saved disabled-but-reading-back-enabled would never
		// refuse anything, with no visible symptom until traffic arrives.
		var raw struct {
			store.ModelRule
			Enabled *bool `json:"enabled"`
		}
		if !decodeBody(w, r, &raw) {
			return
		}
		rule := raw.ModelRule
		rule.Enabled = raw.Enabled == nil || *raw.Enabled
		id, err := s.db.UpsertModelRule(r.Context(), rule)
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		s.reloadRules() // apply immediately, not on the next ticker tick
		saved, err := s.db.GetModelRule(r.Context(), id)
		if err != nil {
			writeJSON(w, http.StatusCreated, map[string]any{"id": id})
			return
		}
		writeJSON(w, http.StatusCreated, saved)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
	}
}

// handleModelRule serves GET / PATCH / DELETE for one rule by id. DELETE
// lifts the restriction entirely (no rule = no cap, no window).
func (s *Server) handleModelRule(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		rule, err := s.db.GetModelRule(r.Context(), id)
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, rule)
	case http.MethodPatch:
		existing, err := s.db.GetModelRule(r.Context(), id)
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		// Merge onto the existing row so a partial PATCH does not zero the
		// fields it omitted (Go's zero-value would silently clear win_start,
		// cap_tokens, etc.).
		var raw struct {
			store.ModelRule
			Enabled *bool `json:"enabled"`
		}
		if !decodeBody(w, r, &raw) {
			return
		}
		merged := *existing
		in := raw.ModelRule
		if in.ModelID != "" {
			merged.ModelID = in.ModelID
		}
		if in.AccountID != 0 {
			merged.AccountID = in.AccountID
		}
		merged.CapTokens = in.CapTokens
		if in.CapWindow != "" {
			merged.CapWindow = in.CapWindow
		}
		// Window fields: a PATCH that omits them ("" everywhere) means "clear
		// the window"; one that sets any of them replaces the window. This is
		// deliberately explicit — partial window updates would produce a
		// half-specified window that validation (correctly) rejects.
		if in.WinStart != "" || in.WinEnd != "" || in.WinDays != "" {
			merged.WinStart, merged.WinEnd, merged.WinDays = in.WinStart, in.WinEnd, in.WinDays
			if in.WinTZ != "" {
				merged.WinTZ = in.WinTZ
			}
		} else {
			merged.WinStart, merged.WinEnd, merged.WinDays = "", "", ""
		}
		if raw.Enabled != nil {
			merged.Enabled = *raw.Enabled
		}
		if in.Note != "" {
			merged.Note = in.Note
		}
		newID, err := s.db.UpsertModelRule(r.Context(), merged)
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		s.reloadRules()
		saved, err := s.db.GetModelRule(r.Context(), newID)
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, saved)
	case http.MethodDelete:
		if err := s.db.DeleteModelRule(r.Context(), id); err != nil {
			s.writeStoreErr(w, err)
			return
		}
		s.reloadRules()
		writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
	}
}
