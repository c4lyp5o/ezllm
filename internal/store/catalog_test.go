package store

import (
	"context"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// UpsertAccount must return a USABLE id on both the insert and the update path.
// Regression: boot seeding passed a local struct with ID=0 into PickKey, which
// failed with "store: not found" and silently left the catalog empty.
func TestUpsertAccountReturnsUsableID(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	acct := provider.Account{
		Name: "SSN GPT", Namespace: "super-ssn", Kind: provider.KindOpenAICompatible,
		BaseURL: "https://space.stationine.com/v1", Enabled: true,
		ProbeDelay: 500 * time.Millisecond, QuotaMode: "probe",
	}

	id1, err := db.UpsertAccount(ctx, acct)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if id1 == 0 {
		t.Fatal("UpsertAccount returned id 0 on the INSERT path")
	}

	// The returned id must actually work for downstream lookups.
	if _, _, err := db.AddKey(ctx, id1, "primary", "sk-a…ef01"); err != nil {
		t.Fatalf("AddKey with returned id: %v", err)
	}
	pick, err := db.PickKey(ctx, id1)
	if err != nil {
		t.Errorf("PickKey(returned id) failed — the id is not usable: %v", err)
	} else if pick.Plaintext != "sk-a…ef01" {
		t.Errorf("PickKey plaintext = %q, want stored test key", pick.Plaintext)
	}

	// Update path (same namespace) must return the SAME id, still usable.
	acct.BaseURL = "https://space.stationine.com/v2"
	acct.Name = "SSN GPT renamed"
	id2, err := db.UpsertAccount(ctx, acct)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if id2 == 0 {
		t.Fatal("UpsertAccount returned id 0 on the ON CONFLICT path (SQLite LastInsertId is 0 when no row is created)")
	}
	if id2 != id1 {
		t.Errorf("upsert created a second row: id %d != %d", id2, id1)
	}
	got, err := db.AccountByID(ctx, id2)
	if err != nil {
		t.Fatalf("AccountByID(returned id): %v", err)
	}
	if got.BaseURL != "https://space.stationine.com/v2" || got.Name != "SSN GPT renamed" {
		t.Errorf("update did not apply: %+v", got)
	}
	// the key must still resolve through the same id
	if _, err := db.PickKey(ctx, id2); err != nil {
		t.Errorf("PickKey after upsert-update: %v", err)
	}

	// Accounts must never duplicate by namespace.
	var n int
	if err := db.Reader().QueryRow(`SELECT COUNT(*) FROM accounts`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("accounts rows = %d, want 1 (namespace must be unique)", n)
	}
}

// Namespace lookup must round-trip every persisted field, including the ones
// the adapter depends on (kind, session-header flag, probe pacing).
func TestAccountByNamespaceRoundTrip(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	in := provider.Account{
		Name: "opencode #1", Namespace: "opengo", Kind: provider.KindOpenCodeGo,
		BaseURL: "https://opencode.ai/zen/go/v1", Enabled: true,
		RequiresSessionHeader: true,
		CustomHeaders:         map[string]string{"User-Agent": "ezllm/1.0"},
		ProbeDelay:            400 * time.Millisecond,
		QuotaMode:             "percent",
	}
	id, err := db.UpsertAccount(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	got, err := db.AccountByNamespace(ctx, "opengo")
	if err != nil {
		t.Fatalf("AccountByNamespace: %v", err)
	}
	if got.ID != id {
		t.Errorf("id = %d, want %d", got.ID, id)
	}
	if got.Kind != provider.KindOpenCodeGo {
		t.Errorf("kind = %q", got.Kind)
	}
	if !got.RequiresSessionHeader {
		t.Error("requires_session_header lost (opencode-go would 400 MissingSessionID)")
	}
	if got.ProbeDelay != 400*time.Millisecond {
		t.Errorf("probe_delay = %v, want 400ms", got.ProbeDelay)
	}
	if got.CustomHeaders["User-Agent"] != "ezllm/1.0" {
		t.Errorf("custom_headers not round-tripped: %+v", got.CustomHeaders)
	}
	if !got.Enabled {
		t.Error("enabled lost")
	}

	// unknown namespace -> ErrNotFound (router turns this into a helpful 404)
	if _, err := db.AccountByNamespace(ctx, "nope"); err != ErrNotFound {
		t.Errorf("unknown namespace err = %v, want ErrNotFound", err)
	}
}

// ModelExists must ALLOW routing when the catalog was never synced (the upstream
// is authoritative), and reject a genuinely unknown model once it is synced.
func TestModelExistsSemantics(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	id, err := db.UpsertAccount(ctx, provider.Account{
		Name: "a", Namespace: "a", Kind: provider.KindOpenAICompatible,
		BaseURL: "https://x/v1", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// empty catalog -> allow
	ok, err := db.ModelExists(ctx, id, "anything")
	if err != nil || !ok {
		t.Errorf("empty catalog should allow routing, got ok=%v err=%v", ok, err)
	}

	if err := db.UpsertModels(ctx, id, []provider.ModelInfo{
		{ID: "gpt-6-luna", OwnedBy: "ssn"}, {ID: "gpt-6.1-sol", OwnedBy: "ssn"},
	}); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"gpt-6-luna", "gpt-6.1-sol"} {
		if ok, err := db.ModelExists(ctx, id, m); err != nil || !ok {
			t.Errorf("ModelExists(%q) = %v, %v; want true", m, ok, err)
		}
	}
	if ok, err := db.ModelExists(ctx, id, "gpt-6-sol"); err != nil || ok {
		t.Errorf("unknown model should not exist, got %v %v", ok, err)
	}

	// re-sync replaces rather than duplicates
	if err := db.UpsertModels(ctx, id, []provider.ModelInfo{{ID: "only-one"}}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.Reader().QueryRow(`SELECT COUNT(*) FROM models WHERE account_id=?`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("models rows = %d after re-sync, want 1", n)
	}
	if ok, _ := db.ModelExists(ctx, id, "gpt-6-luna"); ok {
		t.Error("stale model survived re-sync")
	}
}

// Client tokens are stored hashed; the plaintext must never appear in the DB.
func TestClientTokenHashedAndResolved(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	if err := db.UpsertClientToken(ctx, "hermes", "tok-plain-123", "infer,admin"); err != nil {
		t.Fatal(err)
	}
	name, roles, err := db.ClientByToken(ctx, "tok-plain-123")
	if err != nil {
		t.Fatalf("ClientByToken: %v", err)
	}
	if name != "hermes" {
		t.Errorf("name = %q", name)
	}
	if len(roles) != 2 || roles[1] != "admin" {
		t.Errorf("roles = %v", roles)
	}
	// wrong token -> not found
	if _, _, err := db.ClientByToken(ctx, "wrong"); err != ErrNotFound {
		t.Errorf("wrong token err = %v, want ErrNotFound", err)
	}
	// plaintext must not be in the file
	var hash string
	if err := db.Reader().QueryRow(`SELECT token_hash FROM client_tokens WHERE name='hermes'`).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if hash == "tok-plain-123" || len(hash) != 64 {
		t.Errorf("token not hashed (len %d)", len(hash))
	}
	// disabled tokens stop resolving
	if _, err := db.Writer().Exec(`UPDATE client_tokens SET enabled=0 WHERE name='hermes'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ClientByToken(ctx, "tok-plain-123"); err != ErrNotFound {
		t.Errorf("disabled token still resolved: %v", err)
	}
}

// Namespaces must be listed as "<ns>/<model>" for routing hints and /v1/models.
func TestAllNamespacesAndModelEntries(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	id, err := db.UpsertAccount(ctx, provider.Account{
		Name: "SSN", Namespace: "super-ssn", Kind: provider.KindOpenAICompatible,
		BaseURL: "https://x/v1", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertModels(ctx, id, []provider.ModelInfo{{ID: "gpt-6-luna"}, {ID: "gpt-6.1-sol"}}); err != nil {
		t.Fatal(err)
	}
	ns, err := db.AllNamespaces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ns) != 2 || ns[0] != "super-ssn/gpt-6-luna" {
		t.Errorf("AllNamespaces = %v", ns)
	}
	entries, err := db.ListModelEntries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].ID != "super-ssn/gpt-6-luna" {
		t.Errorf("entries = %+v", entries)
	}
	// a disabled account must drop out of both
	if _, err := db.Writer().Exec(`UPDATE accounts SET enabled=0`); err != nil {
		t.Fatal(err)
	}
	if ns, _ := db.AllNamespaces(ctx); len(ns) != 0 {
		t.Errorf("disabled account still listed: %v", ns)
	}
	if e, _ := db.ListModelEntries(ctx); len(e) != 0 {
		t.Errorf("disabled account still in catalog: %+v", e)
	}
}
