package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/settings"
)

func TestSettingsPersistenceAndCAS(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "settings.sqlite")
	db, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	state := settings.State{Revision: 1, Values: map[string]json.RawMessage{"retries": json.RawMessage(`0`)}}
	if err = db.SaveSettings(ctx, state, 0); err != nil {
		t.Fatal(err)
	}
	if err = db.SaveSettings(ctx, state, 0); !errors.Is(err, settings.ErrConflict) {
		t.Fatal(err)
	}
	db.Close()
	db, err = Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got, err := db.LoadSettings(ctx)
	if err != nil || got.Revision != 1 || string(got.Values["retries"]) != "0" {
		t.Fatal(got, err)
	}
	exp, err := db.ExportConfig(ctx)
	if err != nil || string(exp.Settings["retries"]) != "0" {
		t.Fatal("export lost settings", err)
	}
}
func TestSettingsInheritedUsageDST(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "usage.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// New York's spring transition has a 23-hour day. The same instants fall
	// into different UTC and local days; historical counters stay untouched.
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	for _, ts := range []string{"2026-03-08T04:59:59Z", "2026-03-08T05:00:00Z", "2026-03-09T03:59:59Z", "2026-03-09T04:00:00Z"} {
		stamp, _ := time.Parse(time.RFC3339, ts)
		db.RecordCall(Call{TS: stamp, AccountID: 99, Model: "m", TokensIn: 10})
	}
	if err = db.Flush(); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, loc)
	used, err := db.InheritedRuleUsage(ctx, 99, "m", "daily", now, loc)
	if err != nil || used != 20 {
		t.Fatal(used, err)
	}
	used, err = db.InheritedRuleUsage(ctx, 99, "m", "daily", now, time.UTC)
	if err != nil || used != 20 {
		t.Fatal(used, err)
	}
}
func TestSettingsSessionsOpaqueAndRevoked(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "session.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.CompleteDashboardSetup(ctx, "a-strong-test-password"); err != nil {
		t.Fatal(err)
	}
	token, err := db.DashboardLogin(ctx, "a-strong-test-password")
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.DashboardLogin(ctx, "a-strong-test-password")
	if err != nil {
		t.Fatal(err)
	}
	if err = db.DescribeDashboardSession(ctx, token, "Browser\nTest"); err != nil {
		t.Fatal(err)
	}
	rows, err := db.ListDashboardSessions(ctx, token)
	if err != nil || len(rows) != 2 {
		t.Fatal(rows, err)
	}
	for _, row := range rows {
		if row.ID == token || row.ID == HashToken(token) || len(row.ID) != 32 {
			t.Fatal("session credential exposed")
		}
		if row.Current {
			if row.Browser != "BrowserTest" {
				t.Fatal(row)
			}
			if err = db.RevokeDashboardSessionID(ctx, row.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if db.DashboardSession(ctx, token) || !db.DashboardSession(ctx, other) {
		t.Fatal("individual revocation failed")
	}
	if err = db.RevokeAllDashboardSessions(ctx); err != nil {
		t.Fatal(err)
	}
	if db.DashboardSession(ctx, other) {
		t.Fatal("revoke all failed")
	}
}

func TestSettingsMigrationPreservesLegacyRuleTimezones(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "migration.sqlite")
	db, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	aid, err := db.UpsertAccount(ctx, provider.Account{Name: "test", Namespace: "test", Kind: provider.KindOpenAICompatible, BaseURL: "https://example.invalid", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []ModelRule{{AccountID: aid, ModelID: "implicit", WinTZ: "UTC", Enabled: true}, {AccountID: aid, ModelID: "explicit", WinTZ: "Asia/Kuala_Lumpur", Enabled: true}} {
		if _, err = db.UpsertModelRule(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.Writer().Exec(`UPDATE model_rules SET win_tz='' WHERE model_id='implicit';DELETE FROM schema_version WHERE version>30;INSERT OR IGNORE INTO schema_version(version) VALUES(30)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.ListModelRules(ctx, aid)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		want := "UTC"
		if r.ModelID == "explicit" {
			want = "Asia/Kuala_Lumpur"
		}
		if r.WinTZ != want {
			t.Fatal("migration changed boundaries", r)
		}
	}
	id, err := db.UpsertModelRule(ctx, ModelRule{AccountID: aid, ModelID: "new", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	rule, err := db.GetModelRule(ctx, id)
	if err != nil || rule.WinTZ != "instance" {
		t.Fatal(rule, err)
	}
}
