package server

import (
	"bytes"
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
	"github.com/c4lyp5o/ezllm/internal/store"
)

// ── tokens ──────────────────────────────────────────────────────────────────

// tokenView is a token as the dashboard lists it: the stored policy plus what a
// capped key has used in its current calendar bucket — the same numbers the
// cap enforces, so "budget spent" on the page matches the 429 a client gets.
type tokenView struct {
	store.TokenSummary
	CapUsed  *int64   `json:"cap_used,omitempty"`  // tokens in the current bucket; only when cap_tokens > 0
	SpentUSD *float64 `json:"spent_usd,omitempty"` // priced spend in the current bucket; only when cap_usd > 0
}

func (s *Server) withBudgetUse(ctx context.Context, tokens []store.TokenSummary) []tokenView {
	out := make([]tokenView, len(tokens))
	for i, t := range tokens {
		out[i].TokenSummary = t
		if !t.Enabled {
			continue
		}
		if t.CapTokens > 0 {
			if b, err := s.db.ClientBudgetStatus(ctx, t.Name); err == nil {
				out[i].CapUsed = &b.Used
			}
		}
		if t.CapUSD > 0 {
			if sp, err := s.db.ClientSpendStatus(ctx, t.Name); err == nil {
				out[i].SpentUSD = &sp.Spent
			}
		}
	}
	return out
}

func (s *Server) handleTokens(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		tokens, err := s.db.ListTokens(r.Context())
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, s.withBudgetUse(r.Context(), tokens))
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
			CapUSD        *float64 `json:"cap_usd"`
			CapWindow     string   `json:"cap_window"`
			AllowedModels *string  `json:"allowed_models"`
			ExpiresAt     *string  `json:"expires_at"`
			// Present-and-null clears the profile; absent leaves it alone.
			CompressionProfileRaw json.RawMessage `json:"compression_profile_id"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		// Checked before any write, so a bad cap_usd never half-applies the patch.
		if body.CapUSD != nil && !store.ValidCapUSD(*body.CapUSD) {
			writeErr(w, http.StatusBadRequest, "invalid_request_error", "cap_usd must be a number >= 0")
			return
		}
		var profileID *int64
		if raw := bytes.TrimSpace(body.CompressionProfileRaw); len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
			var v int64
			if err := json.Unmarshal(raw, &v); err != nil || v < 0 {
				writeErr(w, http.StatusBadRequest, "invalid_request_error", "compression_profile_id must be an integer or null")
				return
			}
			if v > 0 {
				if _, err := s.db.GetCompressionProfile(r.Context(), v); err != nil {
					writeErr(w, http.StatusBadRequest, "invalid_request_error", "compression_profile_id names no profile")
					return
				}
				profileID = &v
			}
		}
		if err := s.db.UpdateToken(r.Context(), id, body.Enabled, body.Roles,
			body.CapTokens, body.CapWindow, body.AllowedModels, body.ExpiresAt); err != nil {
			s.writeStoreErr(w, err)
			return
		}
		if body.CapUSD != nil {
			if err := s.db.SetTokenCapUSD(r.Context(), id, *body.CapUSD); err != nil {
				s.writeStoreErr(w, err)
				return
			}
		}
		if len(bytes.TrimSpace(body.CompressionProfileRaw)) > 0 {
			if err := s.db.SetTokenCompressionProfile(r.Context(), id, profileID); err != nil {
				s.writeStoreErr(w, err)
				return
			}
		}
		tokens, _ := s.db.ListTokens(r.Context())
		for _, t := range tokens {
			if t.ID == id {
				// The cap or its window may have changed: re-read spend next time.
				s.forgetSpend(t.Name)
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
	if qErr == nil {
		s.alertQuota(acc.ID, acc.Namespace, q)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"kind": q.Kind, "raw": q.Raw, "read_at": time.Now().UTC().Format(time.RFC3339),
	})
}
