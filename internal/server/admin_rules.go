package server

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/c4lyp5o/ezllm/internal/store"
)

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

// handleModelRulesBatch applies a group edit in one transaction:
// {"upsert":[rule, …], "delete":[id, …]}. Each upsert is a full rule row (an
// omitted `enabled` means enabled, as on create). All or nothing: a failure
// changes no rule and names the model that failed.
func (s *Server) handleModelRulesBatch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Upsert []struct {
			store.ModelRule
			Enabled *bool `json:"enabled"`
		} `json:"upsert"`
		Delete []int64 `json:"delete"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if len(body.Upsert)+len(body.Delete) == 0 {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "nothing to apply: send upsert and/or delete")
		return
	}
	if len(body.Upsert)+len(body.Delete) > maxRuleBatch {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("at most %d rules per batch", maxRuleBatch))
		return
	}
	rules := make([]store.ModelRule, len(body.Upsert))
	for i, u := range body.Upsert {
		rules[i] = u.ModelRule
		rules[i].Enabled = u.Enabled == nil || *u.Enabled
	}
	saved, deleted, err := s.db.ApplyModelRules(r.Context(), rules, body.Delete)
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	s.reloadRules()
	writeJSON(w, http.StatusOK, map[string]int{"saved": saved, "deleted": deleted})
}

// maxRuleBatch bounds one group edit (a big provider has a few hundred models).
const maxRuleBatch = 2000

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
		// Pointers tell "sent" from "left out": a field the PATCH omits keeps
		// its stored value. {"enabled":false} used to zero cap_tokens and wipe
		// the allowed-hours window.
		var raw struct {
			store.ModelRule
			Enabled   *bool   `json:"enabled"`
			CapTokens *int64  `json:"cap_tokens"`
			WinStart  *string `json:"win_start"`
			WinEnd    *string `json:"win_end"`
			WinDays   *string `json:"win_days"`
			Note      *string `json:"note"`
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
		if raw.CapTokens != nil {
			merged.CapTokens = *raw.CapTokens
		}
		if in.CapWindow != "" {
			merged.CapWindow = in.CapWindow
		}
		// Window fields move together: sending any of them replaces the whole
		// window ("" clears that part), because a half-updated window would be
		// half-specified, which validation (correctly) rejects.
		if raw.WinStart != nil || raw.WinEnd != nil || raw.WinDays != nil {
			deref := func(p *string) string {
				if p == nil {
					return ""
				}
				return *p
			}
			merged.WinStart, merged.WinEnd, merged.WinDays = deref(raw.WinStart), deref(raw.WinEnd), deref(raw.WinDays)
		}
		if in.WinTZ != "" {
			merged.WinTZ = in.WinTZ
		}
		if raw.Enabled != nil {
			merged.Enabled = *raw.Enabled
		}
		if raw.Note != nil {
			merged.Note = *raw.Note
		}
		// By id, so a new account or model moves this rule instead of
		// creating a second one beside it.
		if err := s.db.UpdateModelRule(r.Context(), id, merged); err != nil {
			s.writeStoreErr(w, err)
			return
		}
		newID := id
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
