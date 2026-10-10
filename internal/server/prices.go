package server

// v8 (M9) admin surface for the pricing/cost layer. Money is DERIVED at query
// time from model_prices (see internal/store/prices.go), so these handlers are
// thin: read the rate rows, or run the cost join. No rate is ever baked into a
// ledger row — correcting a price must be able to fix history, which is the
// whole reason cost is not a stored column.

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/c4lyp5o/ezllm/internal/store"
)

// handleListPrices returns every versioned rate row.
func (s *Server) handleListPrices(w http.ResponseWriter, r *http.Request) {
	prices, err := s.db.ListPrices(r.Context())
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, prices)
}

// handleSetPrice upserts one rate row for an account+model.
//
// PUT /admin/accounts/{id}/prices
// Body: {"model":"<id>","effective_from":"2026-01-01",
//
//	"price_in":3,"price_out":15,"price_cache_read":0.3,"price_cache_write":3.75,
//	"currency":"USD","note":"..."}
//
// The model id travels in the body, not the path: ids can contain "/" (HF-style).
// effective_from accepts a date or a full timestamp; it is normalized to YYYY-MM-DD.
func (s *Server) handleSetPrice(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Model           string   `json:"model"`
		EffectiveFrom   string   `json:"effective_from"`
		Currency        string   `json:"currency"`
		PriceIn         *float64 `json:"price_in"`
		PriceOut        *float64 `json:"price_out"`
		PriceCacheRead  *float64 `json:"price_cache_read"`
		PriceCacheWrite *float64 `json:"price_cache_write"`
		Note            string   `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", "invalid JSON body")
		return
	}
	if body.Model == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "model is required")
		return
	}
	if body.EffectiveFrom == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "effective_from is required (YYYY-MM-DD)")
		return
	}
	// Every rate must be stated. A missing price is NOT silently zero — that
	// would make a partly-filled row look like "output is free" and quietly
	// understate cost. The operator either states all four or gets a 400.
	if body.PriceIn == nil || body.PriceOut == nil || body.PriceCacheRead == nil || body.PriceCacheWrite == nil {
		writeErr(w, http.StatusBadRequest, "bad_request",
			"price_in, price_out, price_cache_read and price_cache_write are all required")
		return
	}
	// The model must exist in this account's catalog, else a typo silently
	// creates a rate that matches no call and the operator believes they priced
	// something they did not (mirrors the pin endpoint's unknown-model 404).
	entries, err := s.db.ListModels(r.Context(), id)
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	var found bool
	for _, e := range entries {
		if e.ID == body.Model {
			found = true
			break
		}
	}
	if !found {
		writeErr(w, http.StatusNotFound, "not_found", "model not in this account's catalog")
		return
	}

	p := store.Price{
		AccountID:       id,
		ModelID:         body.Model,
		EffectiveFrom:   body.EffectiveFrom,
		Currency:        body.Currency,
		PriceIn:         *body.PriceIn,
		PriceOut:        *body.PriceOut,
		PriceCacheRead:  *body.PriceCacheRead,
		PriceCacheWrite: *body.PriceCacheWrite,
		Note:            body.Note,
	}
	if err := s.db.SetPrice(r.Context(), p); err != nil {
		// Validation errors are the operator's fault (bad date / negative rate).
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	// Echo the stored row (normalized effective_from / currency included).
	echo, _ := s.db.ListPrices(r.Context())
	for _, rp := range echo {
		if rp.AccountID == id && rp.ModelID == body.Model && rp.EffectiveFrom == first10(body.EffectiveFrom) {
			writeJSON(w, http.StatusOK, rp)
			return
		}
	}
	writeJSON(w, http.StatusOK, p)
}

// handleDeletePrice removes one exact rate version. Model + date travel in the
// body (ids can contain "/"). Absent version => 404.
func (s *Server) handleDeletePrice(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Model         string `json:"model"`
		EffectiveFrom string `json:"effective_from"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", "invalid JSON body")
		return
	}
	if body.Model == "" || body.EffectiveFrom == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "model and effective_from are required")
		return
	}
	if err := s.db.DeletePrice(r.Context(), id, body.Model, first10(body.EffectiveFrom)); err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "no such price version")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleCost runs the derived cost report over [from,to), defaulting to the
// last 7 days. The response names the unpriced gap explicitly so a number that
// excludes them is never mistaken for the whole bill.
func (s *Server) handleCost(w http.ResponseWriter, r *http.Request) {
	to := time.Now().UTC()
	from := to.Add(-7 * 24 * time.Hour)
	if v := r.URL.Query().Get("from"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			from = t
		} else {
			writeErr(w, http.StatusBadRequest, "invalid_request_error", "from must be RFC3339")
			return
		}
	}
	if v := r.URL.Query().Get("to"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			to = t
		} else {
			writeErr(w, http.StatusBadRequest, "invalid_request_error", "to must be RFC3339")
			return
		}
	} else {
		to = to.Add(time.Minute) // same ledger-write grace as /admin/usage
	}
	rows, err := s.db.CostReport(r.Context(), from, to)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	var totalUSD float64
	var totalCalls, unpriced int64
	for _, x := range rows {
		totalUSD += x.USD
		totalCalls += x.Calls
		unpriced += x.UnpricedCalls
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"from": from.Format(time.RFC3339), "to": to.Format(time.RFC3339),
		"currency": "USD",
		"rows":     rows,
		"totals": map[string]any{
			"usd":              totalUSD,
			"calls":            totalCalls,
			"unpriced_calls":   unpriced,
			"pricing_complete": unpriced == 0,
			// Honest scope note: unpriced calls are excluded from usd, so a
			// non-zero count means this total is a floor, not the full bill.
			"note": "usd counts priced calls only; see unpriced_calls",
		},
	})
}

func first10(s string) string {
	if len(s) >= 10 {
		return s[:10]
	}
	return s
}
