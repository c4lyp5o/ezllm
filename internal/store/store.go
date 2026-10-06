// Package store: DB open/migrate, writer+reader pools, and the ledger.
//
// SQLite is single-writer. We therefore keep ONE writer connection (serialized
// via a mutex, WAL) and a small read pool. A write pool would only manufacture
// SQLITE_BUSY. Ledger writes are async and batched so they can never add
// latency to a streaming response.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure Go, no CGO — required for the static Docker image
)

//go:embed schema.sql
var schemaSQL string

// schemaVersion must be bumped whenever schema.sql changes incompatibly.
const schemaVersion = 2

// DB wraps the writer/reader split.
type DB struct {
	w    *sql.DB // single writer connection (MaxOpenConns=1)
	r    *sql.DB // read pool
	path string

	writeMu sync.Mutex // serializes writers explicitly (belt and braces over the pool)

	// ledger batching
	callCh  chan ledgerItem
	closeCh chan struct{}
	wg      sync.WaitGroup
	crypto  *FieldCrypto
}

// Options configures Open.
type Options struct {
	Path       string        // sqlite file; parent dirs created
	MasterKey  string        // for FieldCrypto (required)
	BatchSize  int           // ledger flush threshold (default 32)
	BatchWait  time.Duration // ledger flush interval (default 250ms)
	MaxReaders int           // read pool size (default 4)
}

// Open creates/migrates the database and starts the ledger writer goroutine.
func Open(ctx context.Context, opts Options) (*DB, error) {
	if opts.MasterKey == "" {
		return nil, errors.New("store: MasterKey is required")
	}
	if opts.Path == "" {
		opts.Path = filepath.Join("data", "ezllm.sqlite")
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 32
	}
	if opts.BatchWait <= 0 {
		opts.BatchWait = 250 * time.Millisecond
	}
	if opts.MaxReaders <= 0 {
		opts.MaxReaders = 4
	}
	if dir := filepath.Dir(opts.Path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("store: create %s: %w", dir, err)
		}
	}

	dsn := "file:" + opts.Path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"

	w, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open writer: %w", err)
	}
	w.SetMaxOpenConns(1) // THE single writer
	w.SetMaxIdleConns(1)
	w.SetConnMaxLifetime(0)

	r, err := sql.Open("sqlite", dsn+"&mode=ro")
	if err != nil {
		w.Close()
		return nil, fmt.Errorf("store: open reader: %w", err)
	}
	r.SetMaxOpenConns(opts.MaxReaders)
	r.SetMaxIdleConns(opts.MaxReaders)

	crypto, err := NewFieldCrypto(opts.MasterKey)
	if err != nil {
		w.Close()
		r.Close()
		return nil, err
	}

	db := &DB{
		w: w, r: r, path: opts.Path, crypto: crypto,
		callCh: make(chan ledgerItem, 512),

		closeCh: make(chan struct{}),
	}
	if err := db.Ping(ctx); err != nil {
		db.closeConns()
		return nil, err
	}
	if err := db.migrate(); err != nil {
		db.closeConns()
		return nil, err
	}
	db.wg.Add(1)
	go db.ledgerLoop(opts.BatchSize, opts.BatchWait)
	return db, nil
}

// Ping verifies both pools answer.
func (d *DB) Ping(ctx context.Context) error {
	if err := d.w.PingContext(ctx); err != nil {
		return fmt.Errorf("store: writer ping: %w", err)
	}
	if err := d.r.PingContext(ctx); err != nil {
		return fmt.Errorf("store: reader ping: %w", err)
	}
	return nil
}

// hasColumn reports whether a table already has the named column, so the
// non-idempotent ALTER below can be guarded. (schema.sql is re-applied
// wholesale and IS idempotent; ALTER is not, hence this check.)
func hasColumn(ctx context.Context, db *sql.DB, table, col string) (bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, err
		}
		if name == col {
			return true, nil
		}
	}
	return false, rows.Err()
}

// hasTable reports whether a table exists — lets a guarded ALTER no-op (and
// defer to CREATE TABLE) on databases where the table is not there yet.
func hasTable(ctx context.Context, db *sql.DB, table string) (bool, error) {
	var name string
	err := db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name = ?`, table).Scan(&name)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (d *DB) migrate() error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	var cur int
	err := d.w.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&cur)
	if err != nil {
		// schema_version missing -> fresh DB; apply full schema then stamp.
		if _, err := d.w.Exec(schemaSQL); err != nil {
			return fmt.Errorf("store: apply schema: %w", err)
		}
		if err := d.alterCallsAccountID(context.Background()); err != nil {
			return fmt.Errorf("store: alter calls: %w", err)
		}
		if _, err := d.w.Exec(`INSERT INTO schema_version(version) VALUES(?)`, schemaVersion); err != nil {
			return fmt.Errorf("store: stamp version: %w", err)
		}
		return nil
	}
	if cur > schemaVersion {
		return fmt.Errorf("store: database schema version %d is newer than this binary (%d) — refusing to start", cur, schemaVersion)
	}
	if cur < schemaVersion {
		// Order matters: the M5 column must exist BEFORE schema.sql re-runs.
		// schema.sql creates idx_calls_acct_model ON calls(account_id, ...),
		// and on a pre-M5 database CREATE TABLE IF NOT EXISTS is a no-op (the
		// old table lacks account_id) while CREATE INDEX still executes — so
		// running schema.sql first fails with "no such column: account_id"
		// and boot aborts. Adding the column first, then re-applying the
		// idempotent schema, migrates v1 databases cleanly; fresh databases
		// take the branch above, where calls already carries the column.
		if err := d.alterCallsAccountID(context.Background()); err != nil {
			return fmt.Errorf("store: migrate %d->%d alter: %w", cur, schemaVersion, err)
		}
		if _, err := d.w.Exec(schemaSQL); err != nil {
			return fmt.Errorf("store: migrate %d->%d: %w", cur, schemaVersion, err)
		}
		if _, err := d.w.Exec(`INSERT INTO schema_version(version) VALUES(?)`, schemaVersion); err != nil {
			return fmt.Errorf("store: stamp version: %w", err)
		}
	}
	return nil
}

// alterCallsAccountID adds calls.account_id (M5 metering key) on databases
// created before it. Guarded by a pragma check because ALTER TABLE ADD COLUMN
// errors if the column already exists — unlike schema.sql, it is not
// idempotent. New databases get the column straight from schema.sql and this
// is a no-op there.
func (d *DB) alterCallsAccountID(ctx context.Context) error {
	// calls not created yet: schema.sql will create it WITH the column, so
	// there is nothing to add and ALTER would fail ("no such table").
	exists, err := hasTable(ctx, d.w, "calls")
	if err != nil {
		return fmt.Errorf("check calls table: %w", err)
	}
	if !exists {
		return nil
	}
	ok, err := hasColumn(ctx, d.w, "calls", "account_id")
	if err != nil {
		return fmt.Errorf("check calls.account_id: %w", err)
	}
	if ok {
		return nil
	}
	if _, err := d.w.ExecContext(ctx, `ALTER TABLE calls ADD COLUMN account_id INTEGER`); err != nil {
		return fmt.Errorf("add calls.account_id: %w", err)
	}
	return nil
}

// SchemaVersion reports the applied schema version (for /admin/health).
func (d *DB) SchemaVersion() int {
	var v int
	if err := d.r.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&v); err != nil {
		return 0
	}
	return v
}

// Path returns the database file path.
func (d *DB) Path() string { return d.path }

// Crypto exposes field encryption (used by the key-registration flow).
func (d *DB) Crypto() *FieldCrypto { return d.crypto }

// Reader returns the read pool (queries).
func (d *DB) Reader() *sql.DB { return d.r }

// Writer returns the single writer connection. Callers MUST hold writeMu via
// WithWriteTx rather than using this directly for multi-statement work.
func (d *DB) Writer() *sql.DB { return d.w }

// WithWriteTx runs fn inside the serialized writer transaction.
func (d *DB) WithWriteTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	tx, err := d.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Close flushes the ledger and closes both pools.
func (d *DB) Close() error {
	var firstErr error
	// ask the ledger loop to flush + exit, and wait for it
	done := make(chan struct{})
	select {
	case <-d.closeCh: // already closed
	default:
		close(d.closeCh)
	}
	go func() { d.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		firstErr = errors.New("store: ledger flush timed out on close")
	}
	if err := d.closeConns(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

func (d *DB) closeConns() error {
	e1 := d.w.Close()
	e2 := d.r.Close()
	return errors.Join(e1, e2)
}
