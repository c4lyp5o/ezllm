package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/store"
)

func TestPriceLookupAPI(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	if err := h.db.SetPrice(context.Background(), store.Price{
		AccountID: 1, ModelID: "m", EffectiveFrom: "2026-01-01",
		PriceIn: 2.5, PriceOut: 10, PriceCacheRead: 1.25, PriceCacheWrite: 0,
		Source: "manual",
	}); err != nil {
		t.Fatal(err)
	}
	code, _, body := h.do("GET", "/v1/prices?model=m", "tok-infer", "")
	if code != 200 {
		t.Fatalf("prices = %d: %s", code, body)
	}
	var out struct {
		Count  int           `json:"count"`
		Prices []store.Price `json:"prices"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Count != 1 || out.Prices[0].PriceIn != 2.5 {
		t.Fatalf("prices = %+v", out)
	}

	code, _, body = h.do("GET", "/v1/prices?model=m&at=2025-12-31", "tok-infer", "")
	if code != 200 {
		t.Fatalf("historical prices = %d: %s", code, body)
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Count != 0 {
		t.Fatalf("pre-effective lookup returned %+v, want empty", out.Prices)
	}
}

func TestLiteLLMSuffixMap(t *testing.T) {
	in, out := 0.000001, 0.000002
	raw := map[string]liteLLMEntry{
		"openai/gpt-5-nano":      {InputCostPerToken: &in, OutputCostPerToken: &out},
		"deepseek/deepseek-chat": {InputCostPerToken: &in, OutputCostPerToken: &out},
		"no-prices":              {},
	}
	got := litellmSuffixMap(raw)
	if _, ok := got["gpt-5-nano"]; !ok {
		t.Fatal("missing suffix gpt-5-nano")
	}
	if _, ok := got["deepseek-chat"]; !ok {
		t.Fatal("missing suffix deepseek-chat")
	}
	if _, ok := got["no-prices"]; ok {
		t.Fatal("entry without input/output prices must be skipped")
	}
}

func TestPriceBulkImport(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	payload := `{"source":"litellm","prices":[
		{"account_id":1,"model_id":"gpt-6-luna","effective_from":"2026-01-01","price_in":1,"price_out":2,"price_cache_read":0.1,"price_cache_write":0.2},
		{"account_id":1,"model_id":"gpt-6-luna","effective_from":"2026-02-01","price_in":3,"price_out":4,"price_cache_read":0.3,"price_cache_write":0.4}
	]}`
	code, _, body := h.do("POST", "/admin/prices/import", adminTok(), payload)
	if code != 200 {
		t.Fatalf("import = %d: %s", code, body)
	}
	var out struct {
		Imported int    `json:"imported"`
		Source   string `json:"source"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Imported != 2 || out.Source != "litellm" {
		t.Fatalf("import response = %+v", out)
	}
	code, _, body = h.do("GET", "/v1/prices?model=gpt-6-luna", "tok-infer", "")
	if code != 200 {
		t.Fatalf("prices after import = %d: %s", code, body)
	}
}
