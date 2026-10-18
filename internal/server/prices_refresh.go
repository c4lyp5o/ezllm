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

type priceRefreshResult struct {
	Source        string `json:"source"`
	FetchedAt     string `json:"fetched_at"`
	EffectiveFrom string `json:"effective_from"`
	Entries       int    `json:"entries"`
	Matched       int    `json:"matched"`
	Note          string `json:"note"`
}

func normalizeEffective(v string, now time.Time) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return now.UTC().Format("2006-01-02"), nil
	}
	t, err := time.Parse("2006-01-02", v)
	if err != nil {
		return "", fmt.Errorf("effective_from must be YYYY-MM-DD")
	}
	return t.Format("2006-01-02"), nil
}

// refreshPrices is the single implementation behind both the admin endpoint
// and the in-process daily scheduler. Callers validate source/effective_from
// before calling; an empty effective_from means today (UTC).
func (s *Server) refreshPrices(ctx context.Context, source, effective string) (priceRefreshResult, error) {
	if source == "" {
		source = "litellm"
	}
	var err error
	effective, err = normalizeEffective(effective, time.Now())
	if err != nil {
		return priceRefreshResult{}, err
	}
	raw, err := fetchLiteLLMPrices(ctx)
	if err != nil {
		return priceRefreshResult{}, err
	}
	bySuffix := litellmSuffixMap(raw)
	accounts, err := s.db.ListAccounts(ctx)
	if err != nil {
		return priceRefreshResult{}, err
	}
	fetched := time.Now().UTC().Format(time.RFC3339)
	matched := 0
	for _, a := range accounts {
		if !a.Enabled {
			continue
		}
		models, err := s.db.ListModels(ctx, a.ID)
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
				Currency: "USD", Source: source, FetchedAt: fetched,
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
			if err := s.db.SetPrice(ctx, p); err != nil {
				continue
			}
			matched++
		}
	}
	return priceRefreshResult{
		Source: source, FetchedAt: fetched, EffectiveFrom: effective,
		Entries: len(raw), Matched: matched,
		Note: "exact model-id suffix match; unmatched models need manual prices",
	}, nil
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
	effective, err := normalizeEffective(r.URL.Query().Get("effective_from"), time.Now())
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	res, err := s.refreshPrices(r.Context(), source, effective)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// StartPriceRefresh runs the price catalog refresh in-process: once shortly
// after boot, then daily at the given local hour. This is deliberately part of
// the app rather than an external cron — a self-hosted ezllm keeps its own
// prices fresh with no operator scheduler to install.
func (s *Server) StartPriceRefresh(ctx context.Context, hour int, loc *time.Location) {
	if hour < 0 || hour > 23 {
		hour = 3
	}
	if loc == nil {
		loc = time.UTC
	}
	go s.priceRefreshLoop(ctx, hour, loc)
}

func (s *Server) priceRefreshLoop(ctx context.Context, hour int, loc *time.Location) {
	// Give the HTTP server and DB a moment, then fill the catalog so a fresh
	// deploy does not wait until 03:00 for its first prices.
	timer := time.NewTimer(45 * time.Second)
	select {
	case <-ctx.Done():
		timer.Stop()
		return
	case <-timer.C:
	}
	s.runScheduledPriceRefresh(ctx)

	for {
		next := nextDaily(hour, loc)
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		s.runScheduledPriceRefresh(ctx)
	}
}

func (s *Server) runScheduledPriceRefresh(ctx context.Context) {
	res, err := s.refreshPrices(ctx, "litellm", "")
	if err != nil {
		if s.log != nil {
			s.log.Error("scheduled price refresh failed", "err", err)
		}
		return
	}
	if s.log != nil {
		s.log.Info("scheduled price refresh",
			"source", res.Source, "effective_from", res.EffectiveFrom,
			"entries", res.Entries, "matched", res.Matched)
	}
}

func nextDaily(hour int, loc *time.Location) time.Time {
	now := time.Now().In(loc)
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, loc)
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}
