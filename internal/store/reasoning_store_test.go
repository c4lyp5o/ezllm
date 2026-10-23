package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

func seedReasoningModel(t *testing.T, db *DB) int64 {
	t.Helper()
	ctx := context.Background()
	aid, err := db.UpsertAccount(ctx, provider.Account{Name: "r", Namespace: "rn", Kind: provider.KindOpenAICompatible, BaseURL: "https://x.invalid", Enabled: true})
	if err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if err := db.UpsertModels(ctx, aid, []provider.ModelInfo{{ID: "m"}}); err != nil {
		t.Fatalf("seed catalog: %v", err)
	}
	return aid
}

// The round trip that matters: what the proxy learns from a 400 comes back out
// of ModelFacts, which is what the router hands to the hop.
func TestLearnModelReasoningRoundTrips(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "r.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	aid := seedReasoningModel(t, db)

	rule := provider.ReasoningRule{Allowed: []string{"off", "low", "high"}, Required: false, Known: true}
	if err := db.LearnModelReasoning(ctx, aid, "m", rule); err != nil {
		t.Fatalf("learn: %v", err)
	}
	got, err := db.ModelFacts(ctx, aid, "m")
	if err != nil {
		t.Fatalf("facts: %v", err)
	}
	if !got.Reasoning.Known || len(got.Reasoning.Allowed) != 3 || got.Reasoning.Allowed[1] != "low" {
		t.Fatalf("reasoning = %+v, want the three stored values", got.Reasoning)
	}
	if got.Reasoning.Required {
		t.Fatalf("required must stay false for a vocabulary with no mandate")
	}

	// And the clamp it produces is the one the ledger needed.
	if v, drop, changed := got.Reasoning.Clamp("none"); drop || !changed || v != "off" {
		t.Fatalf("Clamp(none) = (%q, drop=%t, changed=%t), want off", v, drop, changed)
	}
}

func TestLearnModelReasoningRecordsAMandate(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "r.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	aid := seedReasoningModel(t, db)

	if err := db.LearnModelReasoning(ctx, aid, "m", provider.ReasoningRule{Required: true, Known: true}); err != nil {
		t.Fatalf("learn: %v", err)
	}
	got, _ := db.ModelFacts(ctx, aid, "m")
	if !got.Reasoning.Required || !got.Reasoning.Known {
		t.Fatalf("reasoning = %+v, want Required", got.Reasoning)
	}
	// Required with no vocabulary: drop the field rather than send 'none'.
	if v, drop, _ := got.Reasoning.Clamp("none"); !drop || v != "" {
		t.Fatalf("Clamp(none) = (%q, drop=%t), want drop", v, drop)
	}
}

// An empty rule must be a no-op: a mis-parse can never erase a known
// vocabulary.
func TestLearnModelReasoningIgnoresAnEmptyRule(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "r.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	aid := seedReasoningModel(t, db)

	if err := db.LearnModelReasoning(ctx, aid, "m", provider.ReasoningRule{Allowed: []string{"low"}, Known: true}); err != nil {
		t.Fatal(err)
	}
	if err := db.LearnModelReasoning(ctx, aid, "m", provider.ReasoningRule{}); err != nil {
		t.Fatal(err)
	}
	got, _ := db.ModelFacts(ctx, aid, "m")
	if len(got.Reasoning.Allowed) != 1 {
		t.Fatalf("an empty rule erased the vocabulary: %+v", got.Reasoning)
	}
}

// The trap the skill names: UpsertModels replaces the catalog WHOLESALE, so any
// column it does not carry in its snapshot is wiped by the next refresh — and a
// refresh runs at every boot.
func TestCatalogRefreshPreservesTheLearnedVocabulary(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "r.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	aid := seedReasoningModel(t, db)

	if err := db.LearnModelReasoning(ctx, aid, "m", provider.ReasoningRule{Allowed: []string{"minimal", "low", "max"}, Known: true}); err != nil {
		t.Fatal(err)
	}
	// A refresh that lists the same model, plus a new one.
	if err := db.UpsertModels(ctx, aid, []provider.ModelInfo{{ID: "m"}, {ID: "m2"}}); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	got, _ := db.ModelFacts(ctx, aid, "m")
	if len(got.Reasoning.Allowed) != 3 {
		t.Fatalf("the catalog refresh wiped the vocabulary: %+v", got.Reasoning)
	}
}

// A database stamped at the previous version must gain the columns through the
// upgrade branch. Dropping them first is the only way to reproduce what a
// deployed database actually looks like: a fresh DB gets both from schema.sql,
// so a bug in the ALTER path would be invisible to it.
func TestReasoningColumnsMigrateFromThePreviousVersion(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "m.sqlite")
	db, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	aid := seedReasoningModel(t, db)
	for _, col := range []string{"reasoning_vocab", "reasoning_required"} {
		if _, err := db.w.ExecContext(ctx, "ALTER TABLE models DROP COLUMN "+col); err != nil {
			t.Fatalf("drop %s (SQLite >= 3.35 required): %v", col, err)
		}
	}
	if _, err = db.w.ExecContext(ctx, `UPDATE schema_version SET version = 28`); err != nil {
		t.Fatalf("stamp 28: %v", err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("reopen at 28: %v", err)
	}
	defer db.Close()
	if got := db.SchemaVersion(); got != schemaVersion {
		t.Fatalf("schema=%d, want %d", got, schemaVersion)
	}
	// The catalog row survived the migration...
	rows, err := db.ListModels(ctx, aid)
	if err != nil || len(rows) != 1 {
		t.Fatalf("catalog after migrate: %v (%d rows)", err, len(rows))
	}
	// ...and the ALTERed columns are usable.
	if err := db.LearnModelReasoning(ctx, aid, "m", provider.ReasoningRule{Allowed: []string{"low", "high"}, Known: true}); err != nil {
		t.Fatalf("learn after migrate: %v", err)
	}
	facts, _ := db.ModelFacts(ctx, aid, "m")
	if len(facts.Reasoning.Allowed) != 2 {
		t.Fatalf("migrated columns unusable: %+v", facts.Reasoning)
	}
}
