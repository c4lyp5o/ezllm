package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/store"
)

func TestNormalizeEffective(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	got, err := normalizeEffective("", now)
	if err != nil || got != "2026-10-18" {
		t.Fatalf("empty = %q, %v; want 2026-10-18, nil", got, err)
	}
	got, err = normalizeEffective("2026-09-18", now)
	if err != nil || got != "2026-09-18" {
		t.Fatalf("date = %q, %v; want 2026-09-18, nil", got, err)
	}
	if _, err := normalizeEffective("18-09-2026", now); err == nil {
		t.Fatal("bad date accepted")
	}
}

func TestNextDailyUsesLocalHour(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Kuala_Lumpur")
	if err != nil {
		t.Skip("tz database unavailable:", err)
	}
	next := nextDaily(3, loc)
	if next.Hour() != 3 || next.Minute() != 0 {
		t.Fatalf("next = %s; want 03:00 local", next)
	}
	if !next.After(time.Now()) {
		t.Fatalf("next = %s; not in the future", next)
	}
}

// A rate the operator set, by hand or by import, is theirs: the daily snapshot
// prices every other model and leaves that one alone, even on the same day.
func TestPriceRefreshKeepsOperatorPrices(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	if err := h.db.UpsertModels(ctx, 1, []provider.ModelInfo{{ID: "gpt-6-luna", OwnedBy: "up"}, {ID: "gpt-6-terra", OwnedBy: "up"}}); err != nil {
		t.Fatal(err)
	}
	today := time.Now().UTC().Format("2006-01-02")
	code, _, body := h.do("PUT", "/admin/accounts/1/prices", adminTok(),
		`{"model":"gpt-6-luna","effective_from":"`+today+`","price_in":5,"price_out":6,"price_cache_read":0.5,"price_cache_write":0.6}`)
	if code != 200 {
		t.Fatalf("set price = %d: %s", code, body)
	}
	cost := func(v float64) *float64 { return &v }
	raw := map[string]liteLLMEntry{
		"openai/gpt-6-luna":  {InputCostPerToken: cost(1e-6), OutputCostPerToken: cost(2e-6)},
		"openai/gpt-6-terra": {InputCostPerToken: cost(3e-6), OutputCostPerToken: cost(4e-6)},
	}
	for run := 1; run <= 2; run++ {
		matched, kept, err := h.server.applyPriceSnapshot(ctx, raw, "litellm", today, time.Now().UTC().Format(time.RFC3339))
		if err != nil || matched != 1 || kept != 1 {
			t.Fatalf("run %d: matched %d kept %d err %v, want 1 written and 1 kept", run, matched, kept, err)
		}
	}
	prices, err := h.db.ListPrices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]store.Price{}
	for _, p := range prices {
		got[p.ModelID] = p
	}
	if p := got["gpt-6-luna"]; p.PriceIn != 5 || p.Source == "litellm" {
		t.Fatalf("operator rate was replaced: %+v", p)
	}
	if p := got["gpt-6-terra"]; p.PriceIn != 3 || p.Source != "litellm" {
		t.Fatalf("snapshot rate missing: %+v", p)
	}
}
