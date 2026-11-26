package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/registration"
	"github.com/c4lyp5o/ezllm/internal/store"
)

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
	if !decodeOptionalBody(w, r, &body) {
		return
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
	if len(surfaces) < len(body.Surfaces) {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "surfaces: each must be chat, anthropic or responses")
		return
	}
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
	readOK := err == nil
	if err := s.db.InsertQuotaSnapshot(r.Context(), keyID, q, readOK, errText(err)); err != nil {
		s.writeStoreErr(w, err)
		return
	}
	if readOK {
		s.alertQuota(acc.ID, acc.Namespace, q)
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
