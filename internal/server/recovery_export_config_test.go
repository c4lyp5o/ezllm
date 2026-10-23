package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/store"
)

func TestRecoveryExportIncludesOperatorConfiguration(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	id := seededAccountID(t, h)
	if _, err := h.db.UpsertModelRule(ctx, store.ModelRule{AccountID: id, ModelID: "gpt-6-luna", CapTokens: 123, CapWindow: "daily", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := h.db.SetPrice(ctx, store.Price{AccountID: id, ModelID: "gpt-6-luna", EffectiveFrom: "2026-01-01", PriceIn: 2}); err != nil {
		t.Fatal(err)
	}
	if err := h.db.SetServiceBinding(ctx, "embeddings", "manual", "up/gpt-6-luna"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Writer().Exec(`UPDATE models SET proto_pin='openai',proto_openai=1 WHERE account_id=?`, id); err != nil {
		t.Fatal(err)
	}
	code, _, body := h.do("GET", "/admin/export", adminTok(), "")
	var out struct {
		Version int               `json:"format_version"`
		Kind    string            `json:"kind"`
		NeedsDB bool              `json:"database_backup_required"`
		Rules   []store.ModelRule `json:"model_rules"`
		Prices  []store.Price     `json:"model_prices"`
		Models  []struct {
			AccountID int64  `json:"account_id"`
			Pin       string `json:"pin"`
		} `json:"models"`
		Bindings []store.ServiceBinding `json:"service_bindings"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if code != 200 || out.Version != 1 || out.Kind != "configuration_export" || !out.NeedsDB {
		t.Fatalf("export contract: %d %s", code, body)
	}
	if len(out.Rules) != 1 || out.Rules[0].CapTokens != 123 || len(out.Prices) != 1 || out.Prices[0].PriceIn != 2 {
		t.Fatalf("rules/prices lost: %s", body)
	}
	if len(out.Models) != 1 || out.Models[0].AccountID != id || out.Models[0].Pin != "openai" {
		t.Fatalf("model pin lost: %s", body)
	}
	if len(out.Bindings) != 1 || out.Bindings[0].ModelTarget != "up/gpt-6-luna" {
		t.Fatalf("binding lost: %s", body)
	}
}
