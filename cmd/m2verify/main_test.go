package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// verifierDB builds a real, migrated database and returns a raw handle onto it
// (the verifier itself only ever reads through database/sql).
func verifierDB(t *testing.T) (*store.DB, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "t.sqlite")
	sdb, err := store.Open(context.Background(), store.Options{Path: path, BatchSize: 1, BatchWait: time.Millisecond, MasterSecret: "test-master-secret"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sdb.Close() })
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return sdb, raw
}

// The usage check replays raw_usage through the CURRENT normalizer: a row
// that billed a cache write twice fails it, a correct row passes, and a row
// whose raw lacks the total is skipped rather than judged.
func TestUsageCheckCatchesADoubleCountedToken(t *testing.T) {
	sdb, raw := verifierDB(t)
	exec := func(q string, args ...any) {
		if _, err := raw.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	insert := func(surface, rawUsage string, in, out, cr, cw int64) {
		exec(`INSERT INTO calls (ts, client, surface, alias, account, model, status, stream, tokens_in, tokens_out, tokens_cached_read, tokens_cached_write, reasoning_tokens, raw_usage)
			VALUES ('2026-10-20T00:00:00Z','c',?, 'a','a','m',200,0,?,?,?,?,0,?)`, surface, in, out, cr, cw, rawUsage)
	}
	resp := `{"input_tokens":29327,"input_tokens_details":{"cache_write_tokens":29324,"cached_tokens":0},"output_tokens":10}`
	insert("responses", resp, 3, 10, 0, 29324)
	insert("anthropic", `{"output_tokens":5}`, 0, 5, 0, 0) // last-event raw: unreplayable
	if r := usageAgreesWithRaw(raw, 100); r.err != nil || !r.ok {
		t.Fatalf("correct rows judged wrong: %+v", r)
	}
	insert("responses", resp, 29327, 10, 0, 29324) // the double count
	if r := usageAgreesWithRaw(raw, 100); r.err != nil || r.ok {
		t.Fatalf("a double-counted cache write passed: %+v", r)
	}
	_ = sdb
}

// Keys written through the store are encrypted; a plaintext key slipped into
// the table directly fails the check.
func TestCredentialCheckRequiresEncryption(t *testing.T) {
	sdb, raw := verifierDB(t)
	ctx := context.Background()
	id, err := sdb.UpsertAccount(ctx, provider.Account{Name: "a", Namespace: "a", Kind: provider.KindOpenAICompatible, BaseURL: "http://x", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := sdb.AddKey(ctx, id, "primary", "sk-real-key-123456"); err != nil {
		t.Fatal(err)
	}
	if r := credentialsEncrypted(raw); r.err != nil || !r.ok {
		t.Fatalf("store-written key judged unencrypted: %+v", r)
	}
	if _, err := raw.Exec(`INSERT INTO provider_keys (account_id, label, key_hint, key_plain) VALUES (?, 'leak', 'sk…', 'sk-plaintext')`, id); err != nil {
		t.Fatal(err)
	}
	if r := credentialsEncrypted(raw); r.ok {
		t.Fatalf("a plaintext key passed: %+v", r)
	}
}
