package server

// Price refresh: pull a public, community-maintained price map and write
// per-account model_prices rows. LiteLLM's model_prices_and_context_window.json
// is the broadest free source; it is a snapshot, not a billing API, so rows are
// stamped source='litellm' and the cost report can show provenance.
//
// Refresh cadence: daily is enough. Provider list prices change on the order of
// weeks, and a daily pass keeps the catalog fresh without hammering the source.
// The endpoint is idempotent for the day (same effective_from upserts).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/c4lyp5o/ezllm/internal/store"
)

const liteLLMPricesURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"

type liteLLMEntry struct {
	InputCostPerToken           *float64 `json:"input_cost_per_token"`
	OutputCostPerToken          *float64 `json:"output_cost_per_token"`
	CacheReadInputTokenCost     *float64 `json:"cache_read_input_token_cost"`
	CacheCreationInputTokenCost *float64 `json:"cache_creation_input_token_cost"`
}

func fetchLiteLLMPrices(ctx context.Context) (map[string]liteLLMEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, liteLLMPricesURL, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 25 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("litellm prices: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	var raw map[string]liteLLMEntry
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("litellm prices: %w", err)
	}
	return raw, nil
}

// litellmSuffixMap indexes entries by the last path segment, which is how most
// account catalogs name a model (`gpt-5-nano`, not `openai/gpt-5-nano`).
// Exact suffix only: a prefix match would price gpt-5-nano with gpt-5's rate.
func litellmSuffixMap(raw map[string]liteLLMEntry) map[string]liteLLMEntry {
	out := make(map[string]liteLLMEntry, len(raw))
	keys := make([]string, 0, len(raw))
	for key := range raw {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		e := raw[key]
		if e.InputCostPerToken == nil || e.OutputCostPerToken == nil {
			continue
		}
		parts := strings.Split(key, "/")
		suffix := strings.ToLower(strings.TrimSpace(parts[len(parts)-1]))
		if suffix == "" {
			continue
		}
		if _, exists := out[suffix]; !exists {
			out[suffix] = e
		}
	}
	return out
}

func (s *Server) handleRefreshPrices(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	source := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("source")))
	if source == "" {
		source = "litellm"
	}
	if source != "litellm" {
		writeErr(w, http.StatusBadRequest, "bad_request", "supported source: litellm")
		return
	}
	raw, err := fetchLiteLLMPrices(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	bySuffix := litellmSuffixMap(raw)
	accounts, err := s.db.ListAccounts(r.Context())
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	effective := time.Now().UTC().Format("2006-01-02")
	if v := strings.TrimSpace(r.URL.Query().Get("effective_from")); v != "" {
		t, err := time.Parse("2006-01-02", v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "effective_from must be YYYY-MM-DD")
			return
		}
		effective = t.Format("2006-01-02")
	}
	fetched := time.Now().UTC().Format(time.RFC3339)
	matched := 0
	for _, a := range accounts {
		if !a.Enabled {
			continue
		}
		models, err := s.db.ListModels(r.Context(), a.ID)
		if err != nil {
			continue
		}
		for _, m := range models {
			modelKey := strings.ToLower(strings.TrimSpace(m.ID))
			e, ok := bySuffix[modelKey]
			if !ok {
				if parts := strings.Split(modelKey, "/"); len(parts) > 1 {
					e, ok = bySuffix[parts[len(parts)-1]]
				}
			}
			if !ok {
				continue
			}
			p := store.Price{
				AccountID: a.ID, ModelID: m.ID, EffectiveFrom: effective,
				Currency: "USD", Source: "litellm", FetchedAt: fetched,
				Note: "litellm snapshot; list price, not a bill",
			}
			if e.InputCostPerToken != nil {
				p.PriceIn = *e.InputCostPerToken * 1e6
			}
			if e.OutputCostPerToken != nil {
				p.PriceOut = *e.OutputCostPerToken * 1e6
			}
			if e.CacheReadInputTokenCost != nil {
				p.PriceCacheRead = *e.CacheReadInputTokenCost * 1e6
			}
			if e.CacheCreationInputTokenCost != nil {
				p.PriceCacheWrite = *e.CacheCreationInputTokenCost * 1e6
			}
			if err := s.db.SetPrice(r.Context(), p); err != nil {
				continue
			}
			matched++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"source": source, "fetched_at": fetched, "effective_from": effective,
		"entries": len(raw), "matched": matched,
		"note": "exact model-id suffix match; unmatched models need manual prices",
	})
}
