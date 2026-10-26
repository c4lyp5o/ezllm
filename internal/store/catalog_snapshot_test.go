package store

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

func snap(t *testing.T, db *DB) *CatalogSnapshot {
	t.Helper()
	s, err := db.CatalogSnapshot(context.Background())
	if err != nil {
		t.Fatalf("CatalogSnapshot: %v", err)
	}
	return s
}

// An unchanged catalog is served from memory; a write to any snapshot table
// is visible to the very next snapshot, whichever store method made it.
func TestCatalogSnapshotSeesEveryWrite(t *testing.T) {
	db := openAt(t, filepath.Join(t.TempDir(), "snap.sqlite"))
	defer db.Close()
	ctx := context.Background()

	first := snap(t, db)
	if again := snap(t, db); again != first {
		t.Fatal("unchanged catalog reloaded the snapshot")
	}

	id, err := db.CreateAccount(ctx, AccountInput{Name: "a", Namespace: "ns", Kind: "openai-compatible",
		BaseURL: "https://x/v1", Enabled: ptrTrue()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := snap(t, db).AccountByNamespace(ctx, "ns"); err != nil {
		t.Fatalf("new account not visible: %v", err)
	}

	keyID, _, err := db.AddKey(ctx, id, "k", "sk-one")
	if err != nil {
		t.Fatal(err)
	}
	if k, err := snap(t, db).PickKey(ctx, id); err != nil || k.Plaintext != "sk-one" {
		t.Fatalf("new key not visible: %+v %v", k, err)
	}
	if err := db.EnableKey(ctx, keyID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := snap(t, db).PickKey(ctx, id); err != ErrNotFound {
		t.Fatalf("disabled key still picked: %v", err)
	}

	if err := db.UpsertModels(ctx, id, []provider.ModelInfo{{ID: "m1"}}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := snap(t, db).ModelExists(ctx, id, "m2"); ok {
		t.Fatal("synced catalog still allows an unknown model")
	}
	no := false
	if err := db.SetModelProto(ctx, id, "m1", provider.SurfaceAnthropic, &no); err != nil {
		t.Fatal(err)
	}
	if v, _ := snap(t, db).ModelSupportsSurface(ctx, id, "m1", provider.SurfaceAnthropic); v == nil || *v {
		t.Fatalf("probe verdict not visible: %v", v)
	}
	if err := db.SetModelPin(ctx, id, "m1", "chat"); err != nil {
		t.Fatal(err)
	}
	if p, _ := snap(t, db).ModelPin(ctx, id, "m1"); p != "chat" {
		t.Fatalf("pin not visible: %q", p)
	}

	comboID, err := db.UpsertCombo(ctx, Combo{Name: "c", Strategy: "failover", Enabled: true,
		Hops: []ComboHop{{AccountID: id, ModelID: "m1", Weight: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	if c, err := snap(t, db).ComboByName(ctx, "c"); err != nil || len(c.Hops) != 1 {
		t.Fatalf("new combo not visible: %+v %v", c, err)
	}
	if err := db.DeleteCombo(ctx, comboID); err != nil {
		t.Fatal(err)
	}
	if _, err := snap(t, db).ComboByName(ctx, "c"); err != ErrNotFound {
		t.Fatalf("deleted combo still served: %v", err)
	}

	in := AccountInput{Name: "a", Namespace: "ns2", Kind: "openai-compatible", BaseURL: "https://x/v1", Enabled: ptrTrue()}
	if err := db.UpdateAccount(ctx, id, in); err != nil {
		t.Fatal(err)
	}
	s := snap(t, db)
	if _, err := s.AccountByNamespace(ctx, "ns"); err != ErrNotFound {
		t.Fatalf("renamed namespace still served: %v", err)
	}
	if err := db.DeleteAccount(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if _, err := snap(t, db).AccountByID(ctx, id); err != ErrNotFound {
		t.Fatalf("deleted account still served: %v", err)
	}
}

// Every snapshot answer equals the live DB method's answer for the same
// question, across present, absent and unsynced rows.
func TestCatalogSnapshotMatchesLiveReads(t *testing.T) {
	db := openAt(t, filepath.Join(t.TempDir(), "parity.sqlite"))
	defer db.Close()
	ctx := context.Background()
	synced, err := db.CreateAccount(ctx, AccountInput{Name: "s", Namespace: "s", Kind: "openai-compatible",
		BaseURL: "https://x/v1", Enabled: ptrTrue(), CustomHeaders: map[string]string{"X-A": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := db.CreateAccount(ctx, AccountInput{Name: "f", Namespace: "f", Kind: "openai-compatible",
		BaseURL: "https://y/v1", Enabled: ptrTrue()})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.AddKey(ctx, synced, "k", "sk-s"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertModels(ctx, synced, []provider.ModelInfo{{ID: "m1"}, {ID: "m2"}}); err != nil {
		t.Fatal(err)
	}
	yes := true
	if err := db.SetModelProto(ctx, synced, "m1", provider.SurfaceChat, &yes); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertCombo(ctx, Combo{Name: "off", Strategy: "failover", Enabled: false,
		Hops: []ComboHop{{AccountID: synced, ModelID: "m1", Weight: 1}}}); err != nil {
		t.Fatal(err)
	}
	s := snap(t, db)

	same := func(what string, a, b any) {
		t.Helper()
		if !reflect.DeepEqual(a, b) {
			t.Errorf("%s: snapshot %#v, live %#v", what, a, b)
		}
	}
	for _, id := range []int64{synced, fresh, 999} {
		sa, se := s.AccountByID(ctx, id)
		la, le := db.AccountByID(ctx, id)
		same("AccountByID", sa, la)
		same("AccountByID err", se, le)
		sk, ske := s.PickKey(ctx, id)
		lk, lke := db.PickKey(ctx, id)
		same("PickKey", sk, lk)
		same("PickKey err", ske, lke)
		for _, m := range []string{"m1", "m2", "nope"} {
			se, _ := s.ModelExists(ctx, id, m)
			le, _ := db.ModelExists(ctx, id, m)
			same("ModelExists "+m, se, le)
			sp, _ := s.ModelPin(ctx, id, m)
			lp, _ := db.ModelPin(ctx, id, m)
			same("ModelPin "+m, sp, lp)
			for _, sf := range []provider.Surface{provider.SurfaceChat, provider.SurfaceAnthropic, provider.SurfaceResponses} {
				sv, _ := s.ModelSupportsSurface(ctx, id, m, sf)
				lv, _ := db.ModelSupportsSurface(ctx, id, m, sf)
				same("ModelSupportsSurface "+m, sv, lv)
			}
		}
	}
	for _, n := range []string{"off", "missing"} {
		sc, se := s.ComboByName(ctx, n)
		lc, le := db.ComboByName(ctx, n)
		same("ComboByName", sc, lc)
		same("ComboByName err", se, le)
	}
	sn, _ := s.AllNamespaces(ctx)
	ln, _ := db.AllNamespaces(ctx)
	same("AllNamespaces", sn, ln)
	scn, _ := s.AllComboNames(ctx)
	lcn, _ := db.AllComboNames(ctx)
	same("AllComboNames", scn, lcn)
}

// PickKey depends on the clock while a key cooldown is live, and no write
// announces its expiry, so the snapshot must ask the DB in that window.
func TestCatalogSnapshotPickKeyIsLiveDuringCooldown(t *testing.T) {
	db := openAt(t, filepath.Join(t.TempDir(), "cool.sqlite"))
	defer db.Close()
	ctx := context.Background()
	id := seedPinAccount(t, db)
	keyID, _, err := db.AddKey(ctx, id, "k", "sk-c")
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(300 * time.Millisecond).UTC().Format("2006-01-02T15:04:05.000Z")
	if _, err := db.w.Exec(`INSERT INTO key_cooldowns (provider_key_id, until_ts, reason) VALUES (?, ?, '429')`, keyID, until); err != nil {
		t.Fatal(err)
	}
	s := snap(t, db)
	if _, err := s.PickKey(ctx, id); err != ErrNotFound {
		t.Fatalf("cooled key picked: %v", err)
	}
	time.Sleep(400 * time.Millisecond)
	if k, err := s.PickKey(ctx, id); err != nil || k.ID != keyID {
		t.Fatalf("key not picked after its cooldown expired: %+v %v", k, err)
	}
}

// A v22 database (no catalog_gen) gains the counter and its triggers on open.
func TestMigrateV22AddsCatalogGen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v22.sqlite")
	db := openAt(t, path)
	for _, q := range []string{
		`DROP TABLE catalog_gen`,
		`DELETE FROM schema_version`,
		`INSERT INTO schema_version(version) VALUES (22)`,
	} {
		if _, err := db.w.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	rows, err := db.w.Query(`SELECT name FROM sqlite_master WHERE type='trigger' AND name LIKE 'catalog_gen_%'`)
	if err != nil {
		t.Fatal(err)
	}
	var triggers []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		triggers = append(triggers, n)
	}
	rows.Close()
	for _, n := range triggers {
		if _, err := db.w.Exec(`DROP TRIGGER ` + n); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	db = openAt(t, path)
	defer db.Close()
	ctx := context.Background()
	before, err := db.catalogGen(ctx)
	if err != nil {
		t.Fatalf("catalog_gen missing after migration: %v", err)
	}
	seedPinAccount(t, db)
	if after, _ := db.catalogGen(ctx); after <= before {
		t.Errorf("catalog_gen = %d after a write, want > %d (triggers missing)", after, before)
	}
}

// A v23 database (no fresh_tool_output) gains the column on open, and every
// existing profile reads "keep" — today's behaviour, unchanged by the upgrade.
func TestMigrateV23AddsFreshToolOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v23.sqlite")
	db := openAt(t, path)
	ctx := context.Background()
	if _, err := db.UpsertCompressionProfile(ctx, CompressionProfile{Name: "old", Enabled: true, Stages: []byte(`[{"engine":"rtk"}]`)}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`ALTER TABLE compression_profiles DROP COLUMN fresh_tool_output`,
		`DELETE FROM schema_version`,
		`INSERT INTO schema_version(version) VALUES (23)`,
	} {
		if _, err := db.w.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	db.Close()

	db = openAt(t, path)
	defer db.Close()
	p, err := db.CompressionProfileByName(ctx, "old")
	if err != nil {
		t.Fatalf("profile unreadable after migration: %v", err)
	}
	if p.FreshToolOutput != "keep" {
		t.Errorf("migrated profile fresh_tool_output = %q, want keep", p.FreshToolOutput)
	}
	p.FreshToolOutput = "compress"
	if _, err := db.UpsertCompressionProfile(ctx, *p); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.CompressionProfileByName(ctx, "old"); got.FreshToolOutput != "compress" {
		t.Errorf("fresh_tool_output did not round-trip: %q", got.FreshToolOutput)
	}
}
