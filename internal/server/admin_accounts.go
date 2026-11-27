package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/registration"
	"github.com/c4lyp5o/ezllm/internal/store"
)

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
	if in.Bridge != "" {
		if strings.TrimSpace(in.APIKey) != "" {
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
