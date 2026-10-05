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
const schemaVersion = 1

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
		if _, err := d.w.Exec(`INSERT INTO schema_version(version) VALUES(?)`, schemaVersion); err != nil {
			return fmt.Errorf("store: stamp version: %w", err)
		}
		return nil
	}
	if cur > schemaVersion {
		return fmt.Errorf("store: database schema version %d is newer than this binary (%d) — refusing to start", cur, schemaVersion)
	}
	if cur < schemaVersion {
		// Idempotent schema (CREATE TABLE IF NOT EXISTS) makes re-application safe
		// for v1; future versions add explicit numbered migrations here.
		if _, err := d.w.Exec(schemaSQL); err != nil {
			return fmt.Errorf("store: migrate %d->%d: %w", cur, schemaVersion, err)
		}
		if _, err := d.w.Exec(`INSERT INTO schema_version(version) VALUES(?)`, schemaVersion); err != nil {
			return fmt.Errorf("store: stamp version: %w", err)
		}
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
