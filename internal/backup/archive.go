// Package backup creates encrypted, verified online SQLite backups.
package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/c4lyp5o/ezllm/internal/store"
)

type Manifest struct {
	Format         string `json:"format"`
	Version        int    `json:"format_version"`
	CreatedAt      string `json:"created_at"`
	Schema         int    `json:"schema_version"`
	DatabaseSHA256 string `json:"database_sha256"`
}

const recoveryInstructions = `Restore with ezllm backup-restore into a new directory while the destination instance is stopped.
The archive includes the database and its active field-encryption master key.
Reconfigure deployment YAML/environment, bridge CLI installations and vendor login directories separately.
Set EZLLM_MASTER_KEY_FILE to the restored ezllm-master.key. Dashboard sessions are revoked on restore.
Keep the separate backup encryption secret; it is not included in this archive.
`

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func writeArchive(ctx context.Context, path, dbPath, master, secret string, schema int) error {
	recipient, err := age.NewScryptRecipient(secret)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	enc, err := age.Encrypt(f, recipient)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(enc)
	tw := tar.NewWriter(gz)
	digest, err := fileDigest(dbPath)
	if err != nil {
		return err
	}
	m := Manifest{Format: "ezllm-encrypted-backup", Version: 1, CreatedAt: time.Now().UTC().Format(time.RFC3339), Schema: schema, DatabaseSHA256: digest}
	raw, _ := json.Marshal(m)
	for _, entry := range []struct {
		name string
		data []byte
	}{{"manifest.json", raw}, {"ezllm-master.key", []byte(master)}, {"RECOVERY.txt", []byte(recoveryInstructions)}} {
		if err = tw.WriteHeader(&tar.Header{Name: entry.name, Mode: 0600, Size: int64(len(entry.data))}); err != nil {
			return err
		}
		if _, err = tw.Write(entry.data); err != nil {
			return err
		}
	}
	db, err := os.Open(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	info, err := db.Stat()
	if err != nil {
		return err
	}
	if err = tw.WriteHeader(&tar.Header{Name: "ezllm.sqlite", Mode: 0600, Size: info.Size()}); err != nil {
		return err
	}
	if _, err = io.Copy(tw, contextReader{ctx, db}); err != nil {
		return err
	}
	if err = tw.Close(); err != nil {
		return err
	}
	if err = gz.Close(); err != nil {
		return err
	}
	if err = enc.Close(); err != nil {
		return err
	}
	return f.Sync()
}

// extract only accepts the versioned fixed archive members. Reading through
// EOF checks age's final authentication tag as well as gzip's checksum.
func extract(ctx context.Context, path, secret, dest string) (Manifest, error) {
	var manifest Manifest
	id, err := age.NewScryptIdentity(secret)
	if err != nil {
		return manifest, err
	}
	f, err := os.Open(path)
	if err != nil {
		return manifest, err
	}
	defer f.Close()
	dec, err := age.Decrypt(contextReader{ctx, f}, id)
	if err != nil {
		return manifest, errors.New("backup decryption failed")
	}
	gz, err := gzip.NewReader(dec)
	if err != nil {
		return manifest, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	seen := map[string]bool{}
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return manifest, err
		}
		switch header.Name {
		case "manifest.json", "ezllm-master.key", "RECOVERY.txt", "ezllm.sqlite":
		default:
			return manifest, errors.New("unexpected archive member")
		}
		if seen[header.Name] || header.Typeflag != tar.TypeReg || header.Size < 0 {
			return manifest, errors.New("invalid archive member")
		}
		seen[header.Name] = true
		if header.Name != "ezllm.sqlite" && header.Size > 65536 {
			return manifest, errors.New("oversized archive metadata")
		}
		out, err := os.OpenFile(filepath.Join(dest, header.Name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return manifest, err
		}
		_, copyErr := io.Copy(out, contextReader{ctx, tr})
		syncErr := out.Sync()
		closeErr := out.Close()
		if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
			return manifest, err
		}
	}
	if len(seen) != 4 {
		return manifest, errors.New("incomplete backup")
	}
	if _, err = io.Copy(io.Discard, contextReader{ctx, gz}); err != nil {
		return manifest, err
	}
	if _, err = io.Copy(io.Discard, contextReader{ctx, dec}); err != nil {
		return manifest, err
	}
	raw, err := os.ReadFile(filepath.Join(dest, "manifest.json"))
	if err != nil {
		return manifest, err
	}
	if err = json.Unmarshal(raw, &manifest); err != nil {
		return manifest, err
	}
	if manifest.Format != "ezllm-encrypted-backup" || manifest.Version != 1 {
		return manifest, errors.New("unsupported backup format")
	}
	digest, err := fileDigest(filepath.Join(dest, "ezllm.sqlite"))
	if err != nil {
		return manifest, err
	}
	if digest != manifest.DatabaseSHA256 {
		return manifest, errors.New("backup database checksum mismatch")
	}
	return manifest, nil
}
func verifyDatabase(ctx context.Context, dir string, m Manifest) error {
	db, err := sql.Open("sqlite", sqliteURI(filepath.Join(dir, "ezllm.sqlite"))+"?mode=ro")
	if err != nil {
		return err
	}
	defer db.Close()
	var result string
	if err = db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return errors.New("backup database integrity check failed")
	}
	var schema int
	if err = db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_version`).Scan(&schema); err != nil {
		return err
	}
	if schema != m.Schema || schema > store.CurrentSchemaVersion() {
		return errors.New("unsupported backup database schema")
	}
	key, err := os.ReadFile(filepath.Join(dir, "ezllm-master.key"))
	if err != nil {
		return err
	}
	crypto, err := store.NewFieldCrypto(strings.TrimSpace(string(key)))
	if err != nil {
		return err
	}
	q := `SELECT key_plain FROM provider_keys UNION ALL SELECT management_key_plain FROM accounts WHERE management_key_plain IS NOT NULL AND management_key_plain<>''`
	// Notification channel secrets (schema 32+) are credentials too; an older
	// backup has no such table.
	var channels int
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='notification_channels'`).Scan(&channels); err != nil {
		return err
	}
	if channels > 0 {
		q += ` UNION ALL SELECT secrets FROM notification_channels WHERE secrets<>''`
	}
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var value string
		if err = rows.Scan(&value); err != nil {
			return err
		}
		if strings.HasPrefix(value, "enc:") {
			if _, err = crypto.Decrypt(value); err != nil {
				return errors.New("backup key cannot decrypt stored credentials")
			}
		}
	}
	return rows.Err()
}
func Verify(ctx context.Context, path, secret string) (Manifest, error) {
	dir, err := os.MkdirTemp("", "ezllm-backup-verify-")
	if err != nil {
		return Manifest{}, err
	}
	defer os.RemoveAll(dir)
	m, err := extract(ctx, path, secret, dir)
	if err == nil {
		err = verifyDatabase(ctx, dir, m)
	}
	return m, err
}

// Restore never writes into an existing destination, even an empty one.
func Restore(ctx context.Context, path, secret, dest string) error {
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		return errors.New("restore destination must not exist")
	}
	parent := filepath.Dir(dest)
	stage, err := os.MkdirTemp(parent, ".ezllm-restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	m, err := extract(ctx, path, secret, stage)
	if err != nil {
		return err
	}
	if err = verifyDatabase(ctx, stage, m); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", sqliteURI(filepath.Join(stage, "ezllm.sqlite")))
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `DELETE FROM dashboard_session_details; DELETE FROM dashboard_sessions`)
	closeErr := db.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	// Rename into a newly reserved destination prevents accidentally replacing an
	// existing empty directory created concurrently by another process.
	if err = os.Mkdir(dest, 0700); err != nil {
		return err
	}
	for _, name := range []string{"ezllm.sqlite", "ezllm-master.key", "manifest.json", "RECOVERY.txt"} {
		if err = os.Rename(filepath.Join(stage, name), filepath.Join(dest, name)); err != nil {
			return fmt.Errorf("restore incomplete: %w", err)
		}
	}
	return syncDir(dest)
}
func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func sqliteURI(path string) string { u := url.URL{Scheme: "file", Path: path}; return u.String() }
