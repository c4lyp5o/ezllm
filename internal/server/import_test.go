package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/store"
)

type importAnswer struct {
	DryRun  bool             `json:"dry_run"`
	Items   []ImportItem     `json:"items"`
	Summary map[string]int   `json:"summary"`
	Error   *json.RawMessage `json:"error"`
}

func doImport(t *testing.T, h *harness, body string, dry bool) (int, importAnswer) {
	t.Helper()
	path := "/admin/import"
	if dry {
		path += "?dry_run=true"
	}
	code, _, resp := h.do("POST", path, "tok-admin", body)
	var out importAnswer
	_ = json.Unmarshal([]byte(resp), &out)
	return code, out
}

func itemFor(items []ImportItem, section, name string) ImportItem {
	for _, it := range items {
		if it.Section == section && it.Name == name {
			return it
		}
	}
	return ImportItem{}
}

// An export imports into another instance: the existing account is updated
// and keeps its key, a new account arrives turned off, the combo's hops point
// at this instance's accounts, its profile and the rule follow by name, and a
// second import changes nothing.
func TestImportRoundTrip(t *testing.T) {
	ctx := context.Background()
	src := settingsHarness(t, func(http.ResponseWriter, *http.Request) {})
	up := seededAccount(t, src)
	acme, err := src.db.CreateAccount(ctx, store.AccountInput{Name: "Acme", Namespace: "acme", Kind: "openai-compatible", BaseURL: "https://api.example.com/v1", Notes: "spare"})
	if err != nil {
		t.Fatal(err)
	}
	if err := src.db.UpdateAccount(ctx, up, store.AccountInput{Notes: "from the source"}); err != nil {
		t.Fatal(err)
	}
	prof, err := src.db.UpsertCompressionProfile(ctx, store.CompressionProfile{Name: "lean", Enabled: true, Stages: json.RawMessage(`[{"engine":"ansi"}]`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.db.UpsertCombo(ctx, store.Combo{Name: "daily", Strategy: "failover", Enabled: true, CompressionProfileID: &prof,
		Hops: []store.ComboHop{{AccountID: up, ModelID: "m"}, {AccountID: acme, ModelID: "gpt-x"}}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.db.ApplyModelRules(ctx, []store.ModelRule{{AccountID: acme, ModelID: "gpt-x", CapTokens: 5000, CapWindow: "daily", WinTZ: "UTC", Enabled: true}}, nil); err != nil {
		t.Fatal(err)
	}
	if code, _, body := src.do("PATCH", "/admin/settings", "tok-admin", `{"revision":0,"values":{"retries":5}}`); code != http.StatusOK {
		t.Fatalf("settings: %d %s", code, body)
	}
	code, _, export := src.do("GET", "/admin/export", "tok-admin", "")
	if code != http.StatusOK {
		t.Fatalf("export: %d", code)
	}

	dst := settingsHarness(t, func(http.ResponseWriter, *http.Request) {})
	// An account the source does not have, so ids differ between the two.
	if _, err := dst.db.CreateAccount(ctx, store.AccountInput{Name: "Other", Namespace: "other", Kind: "openai-compatible", BaseURL: "https://other.example.com/v1"}); err != nil {
		t.Fatal(err)
	}
	var keysBefore int
	_ = dst.db.Reader().QueryRow(`SELECT COUNT(*) FROM provider_keys`).Scan(&keysBefore)

	combosBefore := countTable(t, dst, "combos")
	code, plan := doImport(t, dst, export, true)
	if code != http.StatusOK || !plan.DryRun {
		t.Fatalf("dry run: %d %+v", code, plan)
	}
	for _, want := range []struct{ section, name, action string }{
		{"accounts", "acme", "create"}, {"accounts", "up", "update"},
		{"compression_profiles", "lean", "create"}, {"combos", "daily", "create"},
		{"model_rules", "acme/gpt-x", "create"}, {"settings", "retries", "update"},
	} {
		if got := itemFor(plan.Items, want.section, want.name); got.Action != want.action {
			t.Errorf("%s %s = %+v, want %s", want.section, want.name, got, want.action)
		}
	}
	if n := countTable(t, dst, "combos"); n != combosBefore {
		t.Fatalf("the dry run wrote combos: %d → %d", combosBefore, n)
	}

	if code, out := doImport(t, dst, export, false); code != http.StatusOK || out.DryRun {
		t.Fatalf("apply: %d %+v", code, out)
	}
	var acmeOn bool
	var acmeID, upID int64
	if err := dst.db.Reader().QueryRow(`SELECT id, enabled FROM accounts WHERE namespace='acme'`).Scan(&acmeID, &acmeOn); err != nil || acmeOn {
		t.Fatalf("acme: on=%v err=%v, want created turned off", acmeOn, err)
	}
	_ = dst.db.Reader().QueryRow(`SELECT id FROM accounts WHERE namespace='up'`).Scan(&upID)
	var notes string
	_ = dst.db.Reader().QueryRow(`SELECT COALESCE(notes,'') FROM accounts WHERE id=?`, upID).Scan(&notes)
	if notes != "from the source" {
		t.Errorf("up notes = %q, want the imported ones", notes)
	}
	var keysAfter int
	_ = dst.db.Reader().QueryRow(`SELECT COUNT(*) FROM provider_keys`).Scan(&keysAfter)
	if keysAfter != keysBefore {
		t.Errorf("keys %d → %d: an update must keep the account's keys", keysBefore, keysAfter)
	}
	daily, err := dst.db.ComboByName(ctx, "daily")
	if err != nil || len(daily.Hops) != 2 || daily.Hops[0].AccountID != upID || daily.Hops[1].AccountID != acmeID || daily.CompressionProfileID == nil {
		t.Fatalf("combo = %+v (%v), want hops on this instance's up and acme, with the profile", daily, err)
	}
	rules, _ := dst.db.ListModelRules(ctx, 0)
	if len(rules) != 1 || rules[0].AccountID != acmeID || rules[0].CapTokens != 5000 {
		t.Fatalf("rules = %+v", rules)
	}
	if got := dst.server.settings.Current().Retries; got != 5 {
		t.Errorf("retries = %d, want 5", got)
	}

	_, again := doImport(t, dst, export, true)
	for _, it := range again.Items {
		if it.Action != "unchanged" {
			t.Errorf("second import: %+v, want unchanged", it)
		}
	}
}

// A file that is not a configuration export is refused, and so is a combo
// the file gets wrong, before anything is written.
func TestImportRefusesBadFiles(t *testing.T) {
	h := settingsHarness(t, func(http.ResponseWriter, *http.Request) {})
	if code, _ := doImport(t, h, `{"kind":"something_else"}`, true); code != http.StatusBadRequest {
		t.Fatalf("wrong kind: %d", code)
	}
	if code, _ := doImport(t, h, `not json`, true); code != http.StatusBadRequest {
		t.Fatalf("not json: %d", code)
	}
	bad := `{"kind":"configuration_export","format_version":1,"accounts":[{"id":9,"name":"Acme","namespace":"acme","kind":"openai-compatible","base_url":"https://api.example.com/v1"}],
	  "combos":[{"name":"broken","strategy":"no-such-strategy","hops":[{"account_id":9,"model_id":"m"}]}]}`
	code, _, body := h.do("POST", "/admin/import", "tok-admin", bad)
	if code != http.StatusBadRequest || !strings.Contains(body, "broken") {
		t.Fatalf("bad combo: %d %s", code, body)
	}
	if n := countTable(t, h, "accounts"); n != 1 {
		t.Fatalf("accounts = %d: a refused import wrote something", n)
	}
	if code, _, _ := h.do("POST", "/admin/import", "tok-infer", bad); code != http.StatusForbidden {
		t.Fatalf("infer key: %d", code)
	}
}

// A hop whose account is in neither place is left out and named.
func TestImportNamesDroppedHops(t *testing.T) {
	h := settingsHarness(t, func(http.ResponseWriter, *http.Request) {})
	doc := `{"kind":"configuration_export","format_version":1,"accounts":[],
	  "combos":[{"name":"c","strategy":"failover","enabled":true,"hops":[{"account_id":42,"model_id":"m"}]}]}`
	_, plan := doImport(t, h, doc, true)
	if it := itemFor(plan.Items, "combos", "c"); it.Action != "create" || !strings.Contains(it.Note, "unknown account/m") {
		t.Fatalf("combo item = %+v", it)
	}
}

// A setting this version refuses fails the dry run, before anything is written.
func TestImportChecksSettingsFirst(t *testing.T) {
	h := settingsHarness(t, func(http.ResponseWriter, *http.Request) {})
	doc := `{"kind":"configuration_export","format_version":1,"settings":{"retries":99},
	  "accounts":[{"id":9,"name":"Acme","namespace":"acme","kind":"openai-compatible","base_url":"https://api.example.com/v1"}]}`
	code, _, body := h.do("POST", "/admin/import?dry_run=true", "tok-admin", doc)
	if code != http.StatusBadRequest || !strings.Contains(body, "retries") {
		t.Fatalf("dry run: %d %s", code, body)
	}
}

// A finished import is logged at Info and filtered out at Warn; one that
// stops part way is a Warn.
func TestImportLogLines(t *testing.T) {
	doc := `{"kind":"configuration_export","format_version":1,"accounts":[{"id":9,"name":"Acme","namespace":"acme","kind":"openai-compatible","base_url":"https://api.example.com/v1"}]}`
	for _, tc := range []struct {
		level slog.Level
		want  bool
	}{{slog.LevelInfo, true}, {slog.LevelWarn, false}} {
		var buf bytes.Buffer
		h := newHarness(t, func(http.ResponseWriter, *http.Request) {}, func(o *Options) {
			o.Log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: tc.level}))
		})
		if code, _, body := h.do("POST", "/admin/import", "tok-admin", doc); code != http.StatusOK {
			t.Fatalf("import: %d %s", code, body)
		}
		if got := strings.Contains(buf.String(), "configuration imported"); got != tc.want {
			t.Fatalf("level %v: logged=%v\n%s", tc.level, got, buf.String())
		}
	}
	// The same namespace twice: the second create collides when it is written.
	twice := `{"kind":"configuration_export","format_version":1,"accounts":[
	  {"id":9,"name":"Acme","namespace":"acme","kind":"openai-compatible","base_url":"https://api.example.com/v1"},
	  {"id":10,"name":"Acme 2","namespace":"acme","kind":"openai-compatible","base_url":"https://api.example.com/v1"}]}`
	var buf bytes.Buffer
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {}, func(o *Options) {
		o.Log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	})
	code, _, body := h.do("POST", "/admin/import", "tok-admin", twice)
	if code != http.StatusConflict || !strings.Contains(body, "import again") {
		t.Fatalf("collision: %d %s", code, body)
	}
	if !strings.Contains(buf.String(), "import stopped part way") {
		t.Fatalf("not logged at Warn:\n%s", buf.String())
	}
}
