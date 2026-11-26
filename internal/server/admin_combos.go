package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/proxy"
	"github.com/c4lyp5o/ezllm/internal/store"
)

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
	// Index is the hop's position in the combo (0-based), so a single-hop
	// probe can be told apart from the rest of the chain.
	Index     int    `json:"index"`
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
//   - It SPENDS: one request per hop, max_tokens 16 except on Copilot, non-streaming.
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
	only := -1 // >= 0: probe just that hop
	// -1 is a body sent without a length (chunked): read it too.
	if r.ContentLength != 0 {
		var in struct {
			Surface string `json:"surface"`
			Hop     *int   `json:"hop"`
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
		if in.Hop != nil {
			if *in.Hop < 0 || *in.Hop >= len(combo.Hops) {
				writeErr(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("hop must be 0..%d", len(combo.Hops)-1))
				return
			}
			only = *in.Hop
		}
	}

	hops := make([]comboTestHop, 0, len(combo.Hops))
	for i, h := range combo.Hops {
		if only >= 0 && i != only {
			continue
		}
		hr := comboTestHop{
			Index:     i,
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

		body := probeBodyFor(rt.Account.Kind, h.ModelID)
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

// probeBodyFor adapts the diagnostic request to the provider's supported options.
func probeBodyFor(kind provider.Kind, model string) []byte {
	if kind == provider.KindCopilotBridge {
		m, _ := json.Marshal(model)
		return []byte(fmt.Sprintf(`{"model":%s,"messages":[{"role":"user","content":"ezllm combo test: reply ok"}]}`, m))
	}
	return probeBody(model)
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
			// sticky_idle_s 0 means "never release on idle", so presence, not
			// a zero value, decides whether the field was sent.
			StickyIdleRaw json.RawMessage `json:"sticky_idle_s"`
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
		c.StickyIdleS = cur.StickyIdleS
		if raw := bytes.TrimSpace(body.StickyIdleRaw); len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
			var v int
			if err := json.Unmarshal(raw, &v); err != nil || v < 0 {
				writeErr(w, http.StatusBadRequest, "invalid_request_error", "sticky_idle_s must be a whole number of seconds, 0 or more")
				return
			}
			c.StickyIdleS = v
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
		switch q := r.URL.Query().Get("enabled"); q {
		case "":
		case "true", "false":
			c.Enabled = q == "true"
		default:
			writeErr(w, http.StatusBadRequest, "invalid_request_error", "enabled must be true or false")
			return
		}
		// By id, so a new name renames this combo instead of creating another.
		if err := s.db.UpdateCombo(r.Context(), id, c); err != nil {
			s.writeStoreErr(w, err)
			return
		}
		newID := id
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
