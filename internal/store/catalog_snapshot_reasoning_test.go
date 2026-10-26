package store

import (
	"context"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// The learned reasoning vocabulary must survive the SNAPSHOT path, not just the
// direct row read.
//
// Why this test exists: ModelFacts on *DB selected reasoning_vocab, and the
// resolver's hopFacts reads the rule out of whatever ModelFacts it is given —
// but ResolveCandidates swaps the catalog for a *CatalogSnapshot, whose model
// query never selected the two reasoning columns. So every real request saw an
// empty rule, clampReasoning stayed inert, and each refusal re-taught the same
// vocabulary (live: four "reasoning vocabulary learned" lines for one model in
// 45 minutes, with the same list every time). Two projections of one row, and
// only one of them carried the fact.
func TestCatalogSnapshotCarriesReasoningRule(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	enabled := true
	acct, err := db.CreateAccount(ctx, AccountInput{
		Name: "snap-reasoning", Namespace: "snap-reasoning",
		Kind: string(provider.KindOpenAICompatible), BaseURL: "https://example.test/v1",
		Enabled: &enabled,
	})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	if err := db.UpsertModels(ctx, acct, []provider.ModelInfo{{ID: "deepseek/x-flash", Display: "X Flash"}}); err != nil {
		t.Fatalf("UpsertModels: %v", err)
	}
	rule := provider.ReasoningRule{
		Allowed: []string{"off", "low", "medium", "high", "xhigh", "max"},
		Known:   true,
	}
	if ok, err := db.LearnModelReasoning(ctx, acct, "deepseek/x-flash", rule); err != nil || !ok {
		t.Fatalf("LearnModelReasoning = %v, %v; want the fact to land on the catalog row", ok, err)
	}

	// Direct read: the row really does carry it.
	direct, err := db.ModelFacts(ctx, acct, "deepseek/x-flash")
	if err != nil {
		t.Fatalf("ModelFacts: %v", err)
	}
	if !direct.Reasoning.Known {
		t.Fatalf("direct ModelFacts reasoning = %+v, want the learned rule", direct.Reasoning)
	}

	// Snapshot read: this is the one every request actually uses.
	snap, err := db.CatalogSnapshot(ctx)
	if err != nil {
		t.Fatalf("CatalogSnapshot: %v", err)
	}
	got, err := snap.ModelFacts(ctx, acct, "deepseek/x-flash")
	if err != nil {
		t.Fatalf("snapshot ModelFacts: %v", err)
	}
	if !got.Reasoning.Known {
		t.Fatalf("snapshot ModelFacts reasoning = %+v, want the learned rule — the clamp is inert on every request without it", got.Reasoning)
	}
	if len(got.Reasoning.Allowed) != len(rule.Allowed) {
		t.Fatalf("snapshot Allowed = %v, want %v", got.Reasoning.Allowed, rule.Allowed)
	}
	for i, want := range rule.Allowed {
		if got.Reasoning.Allowed[i] != want {
			t.Fatalf("snapshot Allowed = %v, want %v", got.Reasoning.Allowed, rule.Allowed)
		}
	}
	if got.Reasoning.Required {
		t.Errorf("snapshot Required = true, want false (the provider named a list without mandating it)")
	}
}
