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
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/registration"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// admin wraps a handler in auth + the admin role. 403 without the role, so a
// token that can only infer never sees account/key surfaces.
func (s *Server) admin(next http.HandlerFunc) http.HandlerFunc {
	return s.authed(func(w http.ResponseWriter, r *http.Request) {
		if !hasRole(r, "admin") {
			writeErr(w, http.StatusForbidden, "forbidden", "admin role required")
			return
		}
		next(w, r)
	})
}

// decodeBody reads a bounded JSON body into dst. Unknown fields are rejected:
// a typo'd field name silently ignored is how a "saved" setting never saves.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	limited := io.LimitReader(r.Body, 1<<20)
	dec := json.NewDecoder(limited)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request_error",
			"invalid JSON body: "+err.Error())
		return false
	}
	// Refuse trailing garbage ("{}{}").
	if dec.More() {
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

// handleOverview is the dashboard's single first-paint round trip.
func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	accounts, err := s.db.ListAccounts(ctx)
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	usage, err := s.db.UsageReport(ctx, time.Now().Add(-24*time.Hour), time.Now(), "account")
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	surfaceUsage, err := s.db.UsageReport(ctx, time.Now().Add(-24*time.Hour), time.Now(), "surface")
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	recent, err := s.recentCalls(ctx, 20)
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	health := s.healthSnapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"health":           health,
		"usage":            usage,
		"usage_by_surface": surfaceUsage,
		"accounts":         accounts,
		"recent_calls":     recent,
	})
}

// healthSnapshot counts the same rows /admin/health reports, shaped for the
// dashboard's health strip.
func (s *Server) healthSnapshot() map[string]any {
	var accounts, keys, combos, models, calls int
	var oldest, newest string
	q := func(query string, dest *int) {
		_ = s.db.Reader().QueryRowContext(context.Background(), query).Scan(dest)
	}
	q(`SELECT COUNT(*) FROM accounts`, &accounts)
	q(`SELECT COUNT(*) FROM provider_keys`, &keys)
	q(`SELECT COUNT(*) FROM combos`, &combos)
	q(`SELECT COUNT(*) FROM models`, &models)
	q(`SELECT COUNT(*) FROM calls`, &calls)
	_ = s.db.Reader().QueryRowContext(context.Background(),
		`SELECT COALESCE(MIN(ts),''),COALESCE(MAX(ts),'') FROM calls`).Scan(&oldest, &newest)
	return map[string]any{
		"status":         "ok",
		"schema_version": s.db.SchemaVersion(),
		"db_path":        s.db.Path(),
		"accounts":       accounts,
		"provider_keys":  keys,
		"models":         models,
		"combos":         combos,
		"ledger_rows":    calls,
		"ledger_span":    map[string]string{"oldest": oldest, "newest": newest},
		"dropped_rows":   store.DroppedCalls(),
		"uptime_s":       int(time.Since(s.started).Seconds()),
	}
}

// callsCols is the canonical projection for every calls-ledger list view
// (the dashboard tail, the requests explorer), so both return identical JSON
// keys — tin/tout/cread/... are ledger names in the emitted row maps.
const callsCols = `ts, client, surface, alias, account, model, status, ttft_ms, total_ms,
       tokens_in, tokens_out, tokens_cached_read, tokens_cached_write, reasoning_tokens,
       COALESCE(tokens_saved, 0), COALESCE(compression_profile, ''),
       COALESCE(compression_applied, 0), COALESCE(compression_rules_fired, 0)`

// scanCalls maps rows projected with callsCols into the row shape the
// dashboard and the requests explorer share.
func scanCalls(rows *sql.Rows) ([]map[string]any, error) {
	out := []map[string]any{}
	for rows.Next() {
		var ts, client, surface, alias, account, model string
		var status int
		var ttft, total, tin, tout, cread, cwrite, reasoning any
		var saved, applied, rulesFired int
		var profile string
		if err := rows.Scan(&ts, &client, &surface, &alias, &account, &model, &status,
			&ttft, &total, &tin, &tout, &cread, &cwrite, &reasoning,
			&saved, &profile, &applied, &rulesFired); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"ts": ts, "client": client, "surface": surface, "alias": alias,
			"account": account, "model": model, "status": status,
			"ttft_ms": ttft, "total_ms": total,
			"tin": tin, "tout": tout, "cread": cread, "cwrite": cwrite,
			"reasoning": reasoning,
			"saved":     saved,
			// empty = the request never mentioned compression; "off" /
			// "unknown-profile" / "disabled" = it asked and got no change;
			// otherwise the profile name (applied says whether bytes moved).
			"compression": profile,
			"applied":     applied != 0,
			// non-zero only when caveman prose rules rewrote something
			"rules_fired": rulesFired,
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

	test := s.runTest(r.Context(), acc, in, true)
	if !test.OK {
		writeKeyTestFailure(w, test)
		return
	}

	// Persist after the proof: key row → test result → catalog → quota.
	acct := accountFromSummary(acc)
	keyID, hint, err := s.db.AddKey(r.Context(), accountID, in.Label, in.APIKey)
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	if err := s.persistTest(r.Context(), accountID, keyID, test, hint); err != nil {
		s.writeStoreErr(w, err)
		return
	}
	s.deferProtocol(accountID, keyID, acct, in.APIKey, test, in.ProbeMaxModels)

	keys, _ := s.db.ListKeys(r.Context(), accountID)
	writeJSON(w, http.StatusCreated, map[string]any{
		"key":  findKey(keys, keyID),
		"test": test,
	})
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
	_, pt, err := s.db.KeyForTest(r.Context(), keyID)
	if err != nil {
		s.writeStoreErr(w, err)
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

	test := s.runTest(r.Context(), acc, in, true)
	detail := test.Detail
	if !test.OK {
		// Record the failure against the live row (last_test_ok=0) but keep it.
		_ = s.db.MarkKeyTest(r.Context(), keyID, false, detail)
		writeKeyTestFailure(w, test)
		return
	}
	if err := s.db.MarkKeyTest(r.Context(), keyID, true, detail); err != nil {
		s.writeStoreErr(w, err)
		return
	}
	acct := accountFromSummary(acc)
	if err := s.persistTest(r.Context(), accountID, keyID, test, ""); err != nil {
		s.writeStoreErr(w, err)
		return
	}
	s.deferProtocol(accountID, keyID, acct, pt, test, in.ProbeMaxModels)

	keys, _ := s.db.ListKeys(r.Context(), accountID)
	writeJSON(w, http.StatusOK, map[string]any{
		"key":  findKey(keys, keyID),
		"test": test,
	})
}

// runTest executes the registration flow with sane per-request defaults.
func (s *Server) runTest(ctx context.Context, acc *store.AccountSummary, in keyTestRequest, deferProto bool) *registration.TestResult {
	opts := registration.Options{
		Label:          in.Label,
		APIKey:         in.APIKey,
		SkipInference:  in.SkipInference,
		ProbeMaxModels: in.ProbeMaxModels,
		ProtocolInline: !deferProto,
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
		if err := s.db.UpsertModels(ctx, accountID, test.Catalog); err != nil {
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
	if test.Catalog == nil || len(test.Catalog) == 0 {
		return
	}
	models := test.Catalog
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		res := s.tester.ProtocolSweep(ctx, acct, key, models, registration.Options{ProbeMaxModels: maxModels})
		if res == nil || len(res.Rows) == 0 {
			return
		}
		if err := s.db.SetModelProtos(context.Background(), accountID, res.Rows); err != nil {
			s.log.Error("protocol sweep persist failed", "err", err, "account", acct.Namespace)
		}
		s.log.Info("protocol sweep done",
			"account", acct.Namespace, "models", res.Tested, "support", res.Support)
	}()
}

// writeKeyTestFailure renders the 422 contract from docs/API.md.
func writeKeyTestFailure(w http.ResponseWriter, test *registration.TestResult) {
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
	writeJSON(w, status, map[string]any{"error": e})
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
	if err := s.db.EnableKey(r.Context(), keyID, *body.Enabled); err != nil {
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
	// Optional scope: sync only the models named in the body.
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
		models = scoped
	}
	if err := s.db.UpsertModels(r.Context(), id, models); err != nil {
		s.writeStoreErr(w, err)
		return
	}

	// Protocol sweep: background (paced probes are slow by design).
	surfaces := parseSurfaces(body.Surfaces)
	maxModels := body.Max
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		res := s.tester.ProtocolSweep(ctx, acct, key, models, registration.Options{
			Surfaces:       surfaces,
			ProbeMaxModels: maxModels,
		})
		if res != nil && len(res.Rows) > 0 {
			if err := s.db.SetModelProtos(ctx, id, res.Rows); err != nil {
				s.log.Error("sync protocol sweep persist failed", "err", err)
			}
		}
	}()

	entries, _ := s.db.ListModels(r.Context(), id)
	support := protocolCounts(entries)
	writeJSON(w, http.StatusOK, map[string]any{
		"synced":           len(models),
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

// handleAccountQuota performs a live quota read + snapshot now.
func (s *Server) handleAccountQuota(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	acc, err := s.db.GetAccount(r.Context(), id)
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
	adapter, err := s.registry.Get(accountFromSummary(acc).Kind)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	q, err := adapter.ReadQuota(r.Context(), accountFromSummary(acc), key)
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
			Enabled *bool `json:"enabled"`
		}
		if !decodeBody(w, r, &raw) {
			return
		}
		c := raw.Combo
		c.Enabled = raw.Enabled == nil || *raw.Enabled
		id, err := s.db.UpsertCombo(r.Context(), c)
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
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
		var c store.Combo
		if !decodeBody(w, r, &c) {
			return
		}
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
		if c.CompressionProfileID == nil {
			c.CompressionProfileID = cur.CompressionProfileID
		}
		if c.Hops == nil {
			c.Hops = cur.Hops
		}
		c.Enabled = cur.Enabled
		if r.URL.Query().Get("enabled") != "" {
			c.Enabled = r.URL.Query().Get("enabled") == "true"
		}
		newID, err := s.db.UpsertCombo(r.Context(), c)
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
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
			Enabled   *bool    `json:"enabled"`
			Roles     []string `json:"roles"`
			CapTokens *int64   `json:"cap_tokens"`
			CapWindow string   `json:"cap_window"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		if err := s.db.UpdateToken(r.Context(), id, body.Enabled, body.Roles,
			body.CapTokens, body.CapWindow); err != nil {
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
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	includeLedger := r.URL.Query().Get("include_ledger") == "true"
	exp, err := s.db.ExportAll(r.Context(), includeLedger)
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="ezllm-export-%s.json"`,
			time.Now().UTC().Format("20060102-150405")))
	writeJSON(w, http.StatusOK, exp)
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
		Notes:                 a.Notes,
	}
}

func protocolCounts(entries []store.ModelEntryRow) map[string]int {
	out := map[string]int{"openai": 0, "anthropic": 0, "responses": 0, "untested": 0}
	for _, e := range entries {
		if e.OpenAI != nil && *e.OpenAI {
			out["openai"]++
		}
		if e.Anthropic != nil && *e.Anthropic {
			out["anthropic"]++
		}
		if e.Responses != nil && *e.Responses {
			out["responses"]++
		}
		if e.OpenAI == nil && e.Anthropic == nil && e.Responses == nil {
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
		case "openai", "chat":
			out = append(out, provider.SurfaceOpenAI)
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
