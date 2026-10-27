package router

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// A resolved Route must carry the model's learned reasoning rule.
//
// The clamp is only as good as the rule the ROUTE carries: prepareHopBody
// clamps from rt.Reasoning, and a zero rule means "send the client's body
// untouched". Every existing clamp test builds its Route by hand with the rule
// already attached, so all of them stayed green while the real resolution path
// handed the proxy an empty rule. This asserts at the seam production uses.
func TestResolvedRouteCarriesLearnedReasoningRule(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "ezllm.sqlite")})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	enabled := true
	acct, err := db.CreateAccount(ctx, store.AccountInput{
		Name: "command-code", Namespace: "command-code",
		Kind: string(provider.KindOpenAICompatible), BaseURL: "https://api.command-code.test/v1",
		Enabled: &enabled,
	})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	if _, _, err := db.AddKey(ctx, acct, "primary", "sk-test-reasoning-rule"); err != nil {
		t.Fatalf("AddKey: %v", err)
	}
	if err := db.UpsertModels(ctx, acct, []provider.ModelInfo{{ID: "xiaomi/mimo-v2.6-pro", Display: "MiMo Pro"}}); err != nil {
		t.Fatalf("UpsertModels: %v", err)
	}
	if ok, err := db.LearnModelReasoning(ctx, acct, "xiaomi/mimo-v2.6-pro", provider.ReasoningRule{
		Allowed: []string{"off", "low", "medium", "high", "xhigh", "max"},
		Known:   true,
	}); err != nil || !ok {
		t.Fatalf("LearnModelReasoning = %v, %v", ok, err)
	}

	routes, err := New(db).ResolveCandidates(ctx, "command-code/xiaomi/mimo-v2.6-pro", provider.SurfaceChat, "test")
	if err != nil {
		t.Fatalf("ResolveCandidates: %v", err)
	}
	if len(routes) != 1 {
		t.Fatalf("got %d routes, want 1", len(routes))
	}
	if !routes[0].Reasoning.Known {
		t.Fatalf("route reasoning = %+v, want the learned rule — without it the proxy sends an effort this model refuses", routes[0].Reasoning)
	}
	if len(routes[0].Reasoning.Allowed) != 6 {
		t.Fatalf("route Allowed = %v, want the six-value vocabulary", routes[0].Reasoning.Allowed)
	}
}
