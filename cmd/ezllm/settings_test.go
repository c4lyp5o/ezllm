package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/backup"
	"github.com/c4lyp5o/ezllm/internal/config"
	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/rules"
	"github.com/c4lyp5o/ezllm/internal/settings"
	"github.com/c4lyp5o/ezllm/internal/store"
)

func TestSettingsExplicitYAMLAndEnvironmentPrecedence(t *testing.T) {
	for _, name := range []string{"EZLLM_USAGE_TZ", "EZLLM_PRICE_REFRESH", "EZLLM_COMBO_AUTOPROBE", "EZLLM_TRANSLATION_ENABLED", "EZLLM_TRANSLATION_UNIVERSAL_ENABLED"} {
		t.Setenv(name, "")
	}
	root := t.TempDir()
	file := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(file, []byte("retry:\n  retries: 5\ntranslation:\n  enabled: false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(file)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(context.Background(), store.Options{Path: filepath.Join(root, "db.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m, err := operatorSettings(context.Background(), db, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m.View().Fields["retries"].Source != "yaml" || m.View().Fields["catalog_interval"].Source != "default" {
		t.Fatal(m.View())
	}
	if _, err = m.Patch(context.Background(), 0, map[string]json.RawMessage{"retries": json.RawMessage(`0`), "usage_timezone": json.RawMessage(`"Asia/Kuala_Lumpur"`)}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EZLLM_USAGE_TZ", "America/New_York")
	m, err = operatorSettings(context.Background(), db, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m.Current().Retries != 0 || m.Current().UsageTZ != "America/New_York" || m.View().Fields["usage_timezone"].Editable {
		t.Fatal(m.View())
	}
	if _, err = m.Patch(context.Background(), 1, map[string]json.RawMessage{"usage_timezone": json.RawMessage(`"UTC"`)}); err == nil {
		t.Fatal("environment override editable")
	}
	t.Setenv("EZLLM_USAGE_TZ", "")
	m, err = operatorSettings(context.Background(), db, cfg)
	if err != nil || m.Current().UsageTZ != "Asia/Kuala_Lumpur" {
		t.Fatal(m.Current(), err)
	}
}

func TestSettingsInheritedRuleUsesSnapshotTimezone(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "rules.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	aid, err := db.UpsertAccount(ctx, provider.Account{Name: "test", Namespace: "test", Kind: provider.KindOpenAICompatible, BaseURL: "https://example.invalid", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.UpsertModelRule(ctx, store.ModelRule{AccountID: aid, ModelID: "m", CapWindow: "daily", CapTokens: 15, WinTZ: "instance", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	stamp, _ := time.Parse(time.RFC3339, "2026-10-25T20:00:00Z")
	db.RecordCall(store.Call{TS: stamp, AccountID: aid, Model: "m", TokensIn: 20})
	if err = db.Flush(); err != nil {
		t.Fatal(err)
	}
	now, _ := time.Parse(time.RFC3339, "2026-10-26T01:00:00Z")
	engine, err := rules.NewEngine(storeAdapter{db}, storeAdapter{db}, func() time.Time { return now }, time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	policy := settings.Defaults()
	policy.UsageTZ = "Asia/Kuala_Lumpur"
	admitted := settings.With(ctx, policy)
	policy.UsageTZ = "UTC"
	if err = engine.Evaluate(admitted, aid, "m", "m"); err == nil {
		t.Fatal("inherited cap lost previous local-day usage")
	}
	if err = engine.Evaluate(settings.With(ctx, policy), aid, "m", "m"); err != nil {
		t.Fatal("new timezone should use new calendar range", err)
	}
	// An explicit timezone remains independent of instance changes.
	_, err = db.UpsertModelRule(ctx, store.ModelRule{AccountID: aid, ModelID: "m", CapWindow: "daily", CapTokens: 15, WinTZ: "UTC", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = engine.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if err = engine.Evaluate(admitted, aid, "m", "m"); err != nil {
		t.Fatal("instance timezone changed explicit rule", err)
	}
}

func TestSettingsUnavailableBackupStorageDoesNotBlockPolicyEdits(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "backups.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m, err := settings.New(ctx, db, settings.Defaults(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate saved automation from a previous boot whose volume is now offline.
	if _, err = m.Patch(ctx, 0, map[string]json.RawMessage{"backup_enabled": json.RawMessage(`true`)}); err != nil {
		t.Fatal(err)
	}
	m.Check = backupSettingsCheck(m, backup.New(db, "", "", "test-master"))
	if _, err = m.Patch(ctx, 1, map[string]json.RawMessage{"retries": json.RawMessage(`0`)}); err != nil {
		t.Fatal("offline backups blocked routing policy", err)
	}
	if _, err = m.Patch(ctx, 2, map[string]json.RawMessage{"backup_enabled": json.RawMessage(`false`)}); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Patch(ctx, 3, map[string]json.RawMessage{"backup_enabled": json.RawMessage(`true`)}); err == nil {
		t.Fatal("enabling backups without storage succeeded")
	}
}
