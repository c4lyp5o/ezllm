package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestRecoveryOnlineBackupIncludesWAL(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python 3 required for optional online backup tool")
	}
	db := openTest(t)
	ctx := context.Background()
	if _, err = db.w.Exec(`PRAGMA wal_checkpoint(TRUNCATE); PRAGMA wal_autocheckpoint=0`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.AddClientToken(ctx, "committed-in-wal", "dummy-token", "infer"); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	key := filepath.Join(root, "active.key")
	if err = os.WriteFile(key, []byte("dummy-master-key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "backup")
	runCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	args := []string{"../../scripts/backup_sqlite.py", "--db", db.Path(), "--key", key, "--output", output}
	if b, err := exec.CommandContext(runCtx, python, args...).CombinedOutput(); err != nil {
		t.Fatalf("backup: %v %s", err, b)
	}
	rawManifest, err := os.ReadFile(filepath.Join(output, "COMPLETE.json"))
	if err != nil {
		t.Fatal(err)
	}
	var checksum struct {
		SHA string `json:"database_sha256"`
	}
	if err = json.Unmarshal(rawManifest, &checksum); err != nil {
		t.Fatal(err)
	}
	rawDB, err := os.ReadFile(filepath.Join(output, "ezllm.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%x", sha256.Sum256(rawDB)) != checksum.SHA {
		t.Fatal("completion hash does not match closed database snapshot")
	}
	restored, err := Open(ctx, Options{Path: filepath.Join(output, "ezllm.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	var n int
	if err = restored.r.QueryRow(`SELECT COUNT(*) FROM client_tokens WHERE name='committed-in-wal'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("WAL row lost: %d %v", n, err)
	}
	b, err := os.ReadFile(filepath.Join(output, "COMPLETE.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Complete bool `json:"complete"`
		Schema   int  `json:"schema_version"`
	}
	if err = json.Unmarshal(b, &manifest); err != nil || !manifest.Complete || manifest.Schema != schemaVersion {
		t.Fatalf("completion marker invalid: %s %v", b, err)
	}
	b, err = os.ReadFile(filepath.Join(output, "ezllm-master.key"))
	if err != nil || string(b) != "dummy-master-key\n" {
		t.Fatal("key copy changed")
	}
	if b, err := exec.CommandContext(runCtx, python, args...).CombinedOutput(); err == nil {
		t.Fatalf("existing backup replaced: %s", b)
	}
}
