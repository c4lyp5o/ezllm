package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// RotateResult counts the values RotateMasterKey re-encrypted, per column.
type RotateResult struct {
	ProviderKeys   int
	ManagementKeys int
	ChannelSecrets int
	WasPlaintext   int // values that were stored unencrypted and are now encrypted
}

// secretColumn is one column that holds secrets at rest.
type secretColumn struct {
	table, column string
	count         func(*RotateResult) *int
}

var secretColumns = []secretColumn{
	{"provider_keys", "key_plain", func(r *RotateResult) *int { return &r.ProviderKeys }},
	{"accounts", "management_key_plain", func(r *RotateResult) *int { return &r.ManagementKeys }},
	{"notification_channels", "secrets", func(r *RotateResult) *int { return &r.ChannelSecrets }},
}

// RotateMasterKey re-encrypts every stored secret from oldSecret to newSecret
// in one transaction on the database at path. ezllm must be stopped: a running
// instance holds the old key and could write a value under it mid-rotation.
//
// Every value is decrypted with the old key, encrypted with the new one, and
// decrypted again with the new one before anything commits; any failure
// (a value the old key cannot open, a mismatch) rolls the whole rotation back.
// A value still stored unencrypted is encrypted under the new key.
func RotateMasterKey(ctx context.Context, path, oldSecret, newSecret string) (RotateResult, error) {
	var res RotateResult
	if strings.TrimSpace(oldSecret) == strings.TrimSpace(newSecret) {
		return res, fmt.Errorf("store: the new master key is the same as the old one")
	}
	oldC, err := NewFieldCrypto(oldSecret)
	if err != nil {
		return res, fmt.Errorf("store: old master key: %w", err)
	}
	newC, err := NewFieldCrypto(newSecret)
	if err != nil {
		return res, fmt.Errorf("store: new master key: %w", err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=rw&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)")
	if err != nil {
		return res, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		return res, err
	}
	defer conn.Close()
	// IMMEDIATE takes the write lock up front: no other writer can slip a
	// value in under the old key between our read and our update.
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return res, fmt.Errorf("store: lock the database (is ezllm still running?): %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	for _, col := range secretColumns {
		n, plain, err := rotateColumn(ctx, conn, col, oldC, newC)
		if err != nil {
			return RotateResult{}, err
		}
		*col.count(&res) = n
		res.WasPlaintext += plain
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return RotateResult{}, fmt.Errorf("store: commit the rotation: %w", err)
	}
	committed = true
	return res, nil
}

func rotateColumn(ctx context.Context, conn *sql.Conn, col secretColumn, oldC, newC *FieldCrypto) (rotated, wasPlain int, err error) {
	rows, err := conn.QueryContext(ctx, fmt.Sprintf(`SELECT rowid, %s FROM %s WHERE %s IS NOT NULL AND %s != ''`, col.column, col.table, col.column, col.column))
	if err != nil {
		return 0, 0, err
	}
	type pending struct {
		id  int64
		enc string
	}
	var updates []pending
	for rows.Next() {
		var id int64
		var stored string
		if err := rows.Scan(&id, &stored); err != nil {
			rows.Close()
			return 0, 0, err
		}
		plain := stored
		if strings.HasPrefix(stored, cryptoPrefix) {
			if plain, err = oldC.Decrypt(stored); err != nil {
				rows.Close()
				return 0, 0, fmt.Errorf("store: %s.%s row %d does not open with the old master key: %w", col.table, col.column, id, err)
			}
		} else {
			wasPlain++
		}
		enc, err := newC.Encrypt(plain)
		if err != nil {
			rows.Close()
			return 0, 0, err
		}
		if back, err := newC.Decrypt(enc); err != nil || back != plain {
			rows.Close()
			return 0, 0, fmt.Errorf("store: %s.%s row %d did not survive re-encryption", col.table, col.column, id)
		}
		updates = append(updates, pending{id, enc})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, 0, err
	}
	rows.Close()
	for _, u := range updates {
		if _, err := conn.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET %s=? WHERE rowid=?`, col.table, col.column), u.enc, u.id); err != nil {
			return 0, 0, err
		}
	}
	return len(updates), wasPlain, nil
}
