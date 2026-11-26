package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/store"
)

// The command rotates the database and swaps the key file, keeping the old key.
func TestRotateMasterKeyCommand(t *testing.T) {
	t.Setenv("EZLLM_MASTER_KEY", "")
	t.Setenv("EZLLM_MASTER_KEY_FILE", "")
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "ezllm-master.key")
	if err := os.WriteFile(keyPath, []byte("old-master-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(dir, "ezllm.sqlite"), MasterSecret: "old-master-secret"})
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateAccount(ctx, store.AccountInput{Name: "Acme", Namespace: "acme", Kind: "openai-compatible", BaseURL: "https://api.example.com/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.AddKey(ctx, id, "main", "sk-provider-secret"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	var out bytes.Buffer
	if err := runRotateMasterKey([]string{"--data-dir", dir}, &out); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	newKey, _ := os.ReadFile(keyPath)
	if strings.TrimSpace(string(newKey)) == "old-master-secret" || len(strings.TrimSpace(string(newKey))) < 32 {
		t.Fatalf("key file not replaced with a fresh key: %q", newKey)
	}
	if fi, err := os.Stat(keyPath); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("new key file mode = %v, %v; want 0600", fi.Mode().Perm(), err)
	}
	olds, _ := filepath.Glob(keyPath + ".old-*")
	if len(olds) != 1 {
		t.Fatalf("old key copies = %v, want one", olds)
	}
	db, err = store.Open(ctx, store.Options{Path: filepath.Join(dir, "ezllm.sqlite"), MasterSecret: strings.TrimSpace(string(newKey))})
	if err != nil {
		t.Fatalf("the new key file does not open the database: %v", err)
	}
	pick, err := db.PickKey(ctx, id)
	db.Close()
	if err != nil || pick.Plaintext != "sk-provider-secret" {
		t.Fatalf("provider key after rotation = %q, %v", pick.Plaintext, err)
	}
	if !strings.Contains(out.String(), "1 provider keys") {
		t.Fatalf("output = %q", out.String())
	}
}

// A key held only in EZLLM_MASTER_KEY cannot be rotated in place.
func TestRotateMasterKeyRefusesEnvKey(t *testing.T) {
	t.Setenv("EZLLM_MASTER_KEY", "from-env")
	if err := runRotateMasterKey([]string{"--data-dir", t.TempDir()}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "EZLLM_MASTER_KEY") {
		t.Fatalf("err = %v", err)
	}
}

// token create prints a key that authenticates, once, and refuses a taken name.
func TestTokenCreateCommand(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if err := runTokenCreate([]string{"create", "--data-dir", dir, "--name", "my-agent", "--roles", "infer,metrics"}, &out); err != nil {
		t.Fatal(err)
	}
	plaintext := strings.TrimSpace(out.String())
	ctx := context.Background()
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(dir, "ezllm.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	name, roles, err := db.ClientByToken(ctx, plaintext)
	db.Close()
	if err != nil || name != "my-agent" || strings.Join(roles, ",") != "infer,metrics" {
		t.Fatalf("ClientByToken = %q %v %v", name, roles, err)
	}
	if err := runTokenCreate([]string{"create", "--data-dir", dir, "--name", "my-agent"}, &bytes.Buffer{}); err == nil {
		t.Fatal("a taken name must be refused")
	}
	if err := runTokenCreate([]string{"list"}, &bytes.Buffer{}); err == nil {
		t.Fatal("only `token create` exists")
	}
}
