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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/c4lyp5o/ezllm/internal/settings"
	_ "modernc.org/sqlite" // pure Go, no CGO — required for the static Docker image
)

//go:embed schema.sql
var schemaSQL string

// schemaVersion must be bumped whenever schema.sql changes incompatibly.
const schemaVersion = 31

// DB wraps the writer/reader split.
type DB struct {
	operatorSettings atomic.Pointer[settings.Manager]
	w                *sql.DB // single writer connection (MaxOpenConns=1)
	r                *sql.DB // read pool
	path             string

	writeMu sync.Mutex // serializes writers explicitly (belt and braces over the pool)

	// committed-batch fan-out for live consumers (see events.go)
	sinkMu sync.Mutex
	subs   []chan []Call

	// ledger batching
	callCh             chan ledgerItem
	reservationCleanup chan int64
	closeCh            chan struct{}
	// recMu orders RecordCall against Close: a row enqueued under RLock is
	// in callCh before closeCh closes (so the final drain sees it), and a row
	// arriving after Close sees closed and is counted as dropped.
	recMu  sync.RWMutex
	closed bool
	wg     sync.WaitGroup

	// crypto at rest for provider keys (nil = plaintext, dev/back-compat)
	crypto *FieldCrypto

	// routing-catalog snapshot (catalog_snapshot.go); catalogMu serializes reloads
	catalogMu sync.Mutex
	catalog   atomic.Pointer[CatalogSnapshot]

	// unix nanos of the last expired-reservation prune (client_budget.go)
	lastReservationSweep atomic.Int64
}

// Options configures Open.
type Options struct {
	Path         string        // sqlite file; parent dirs created
	BatchSize    int           // ledger flush threshold (default 32)
	BatchWait    time.Duration // ledger flush interval (default 250ms)
	MaxReaders   int           // read pool size (default 4)
	MasterSecret string        // master key for at-rest provider-key encryption; empty = plaintext (dev)
}

// Open creates/migrates the database and starts the ledger writer goroutine.
func Open(ctx context.Context, opts Options) (*DB, error) {
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

	db := &DB{
		w: w, r: r, path: opts.Path,
		callCh:             make(chan ledgerItem, 512),
		reservationCleanup: make(chan int64, 512),

		closeCh: make(chan struct{}),
	}
	if strings.TrimSpace(opts.MasterSecret) != "" {
		fc, err := NewFieldCrypto(opts.MasterSecret)
		if err != nil {
			db.closeConns()
			return nil, err
		}
		db.crypto = fc
	}
	if err := db.Ping(ctx); err != nil {
		db.closeConns()
		return nil, err
	}
	if err := db.migrate(); err != nil {
		db.closeConns()
		return nil, err
	}
	if _, err := db.w.Exec(`INSERT OR IGNORE INTO dashboard_session_details(token_hash,id) SELECT token_hash,lower(hex(randomblob(16))) FROM dashboard_sessions`); err != nil {
		db.closeConns()
		return nil, err
	}

	if db.crypto != nil {
		if err := db.migrateEncryptProviderKeys(ctx); err != nil {
			db.closeConns()
			return nil, fmt.Errorf("store: encrypt existing provider keys: %w", err)
		}
	}
	if err := db.EnsureDashboardPassword(ctx); err != nil {
		db.closeConns()
		return nil, fmt.Errorf("store: initialize dashboard password: %w", err)
	}
	db.wg.Add(2)
	go db.reservationCleanupLoop()
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
type schemaReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func hasColumn(ctx context.Context, db schemaReader, table, col string) (bool, error) {
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
func hasTable(ctx context.Context, db schemaReader, table string) (bool, error) {
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
		if err := d.alterCallsRulesFired(context.Background()); err != nil {
			return fmt.Errorf("store: alter calls rules_fired: %w", err)
		}
		if err := d.alterCallsContextTokens(context.Background()); err != nil {
			return fmt.Errorf("store: alter calls context tokens: %w", err)
		}
		if err := d.alterCallsCompressionVersion(context.Background()); err != nil {
			return fmt.Errorf("store: alter calls compression version: %w", err)
		}
		if err := d.alterCallsCompressionAttribution(context.Background()); err != nil {
			return fmt.Errorf("store: alter calls compression attribution: %w", err)
		}
		if err := d.alterAccountMaxContext(context.Background()); err != nil {
			return fmt.Errorf("store: alter accounts max context: %w", err)
		}
		if err := d.alterDashboardAuth(context.Background()); err != nil {
			return fmt.Errorf("store: alter dashboard auth: %w", err)
		}
		if err := d.alterAccountRate(context.Background()); err != nil {
			return fmt.Errorf("store: alter account rate: %w", err)
		}
		if err := d.alterAccountUsageConvention(context.Background()); err != nil {
			return fmt.Errorf("store: alter account messages usage: %w", err)
		}
		if err := d.alterAccountManagementKey(context.Background()); err != nil {
			return fmt.Errorf("store: alter account management key: %w", err)
		}
		if err := d.alterComboHopHold(context.Background()); err != nil {
			return fmt.Errorf("store: alter combo hop hold: %w", err)
		}
		if err := d.alterCallsIP(context.Background()); err != nil {
			return fmt.Errorf("store: alter calls ip: %w", err)
		}
		if err := d.rebuildCombosV9(context.Background()); err != nil {
			return fmt.Errorf("store: rebuild combos: %w", err)
		}
		if err := d.alterComboAutoFree(context.Background()); err != nil {
			return fmt.Errorf("store: alter combo auto_free: %w", err)
		}
		if err := d.alterAccountCatalogSynced(context.Background()); err != nil {
			return fmt.Errorf("store: alter accounts catalog_synced_at: %w", err)
		}
		if _, err := d.w.Exec(schemaSQL); err != nil {
			return fmt.Errorf("store: reapply schema: %w", err)
		}
		if err := d.seedFreeCombo(context.Background()); err != nil {
			return fmt.Errorf("store: seed free combo: %w", err)
		}
		if _, err := d.w.Exec(`INSERT INTO schema_version(version) VALUES(?)`, schemaVersion); err != nil {
			return fmt.Errorf("store: stamp version: %w", err)
		}
		return nil
	}
	if cur > schemaVersion {
		return fmt.Errorf("store: database schema version %d is newer than this binary (%d) — refusing to start", cur, schemaVersion)
	}
	// Resume an interrupted legacy rebuild even if an old process stamped
	// the current version after recreating an empty table. Ambiguity fails closed.
	if err := d.rebuildCombosV9(context.Background()); err != nil {
		return err
	}
	if cur < schemaVersion {
		// Order matters: legacy columns must exist BEFORE schema.sql re-runs.
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
		if err := d.alterCallsRulesFired(context.Background()); err != nil {
			return fmt.Errorf("store: alter calls rules_fired: %w", err)
		}
		if err := d.alterCallsContextTokens(context.Background()); err != nil {
			return fmt.Errorf("store: alter calls context tokens: %w", err)
		}
		if err := d.alterCallsCompressionVersion(context.Background()); err != nil {
			return fmt.Errorf("store: migrate calls compression version: %w", err)
		}
		if err := d.alterCallsCompressionAttribution(context.Background()); err != nil {
			return fmt.Errorf("store: migrate calls compression attribution: %w", err)
		}
		if err := d.alterAccountMaxContext(context.Background()); err != nil {
			return fmt.Errorf("store: migrate accounts max context: %w", err)
		}
		if err := d.alterDashboardAuth(context.Background()); err != nil {
			return fmt.Errorf("store: migrate dashboard auth: %w", err)
		}
		if err := d.alterComboContextSize(context.Background()); err != nil {
			return fmt.Errorf("store: migrate combo context size: %w", err)
		}
		if err := d.alterModelProtoPin(context.Background()); err != nil {
			return fmt.Errorf("store: migrate model proto pin: %w", err)
		}
		if err := d.alterModelProtoSource(context.Background()); err != nil {
			return fmt.Errorf("store: migrate model proto source: %w", err)
		}
		if err := d.alterModelReasoning(context.Background()); err != nil {
			return fmt.Errorf("store: migrate model reasoning vocabulary: %w", err)
		}
		if err := d.alterCallsResearch(context.Background()); err != nil {
			return fmt.Errorf("store: migrate calls research columns: %w", err)
		}
		if err := d.alterCallsTranslated(context.Background()); err != nil {
			return fmt.Errorf("store: alter calls translated: %w", err)
		}
		if err := d.alterAccountRate(context.Background()); err != nil {
			return fmt.Errorf("store: migrate account rate: %w", err)
		}
		if err := d.alterAccountUsageConvention(context.Background()); err != nil {
			return fmt.Errorf("store: migrate account messages usage: %w", err)
		}
		if err := d.alterAccountManagementKey(context.Background()); err != nil {
			return fmt.Errorf("store: migrate account management key: %w", err)
		}
		if err := d.alterComboHopHold(context.Background()); err != nil {
			return fmt.Errorf("store: migrate combo hop hold: %w", err)
		}
		if err := d.alterCallsIP(context.Background()); err != nil {
			return fmt.Errorf("store: migrate calls ip: %w", err)
		}
		if err := d.alterClientTokenPolicy(context.Background()); err != nil {
			return fmt.Errorf("store: migrate client token policy: %w", err)
		}
		if err := d.alterModelPriceSource(context.Background()); err != nil {
			return fmt.Errorf("store: migrate model price source: %w", err)
		}
		if err := d.alterProfileFreshToolOutput(context.Background()); err != nil {
			return fmt.Errorf("store: migrate profile fresh_tool_output: %w", err)
		}
		if err := d.rebuildCombosV9(context.Background()); err != nil {
			return fmt.Errorf("store: migrate combos: %w", err)
		}
		if err := d.alterComboAutoFree(context.Background()); err != nil {
			return fmt.Errorf("store: alter combo auto_free: %w", err)
		}
		if err := d.alterAccountCatalogSynced(context.Background()); err != nil {
			return fmt.Errorf("store: alter accounts catalog_synced_at: %w", err)
		}
		if err := d.renameWireOpenAIToChat(context.Background()); err != nil {
			return fmt.Errorf("store: rename wire openai to chat: %w", err)
		}
		if _, err := d.w.Exec(schemaSQL); err != nil {
			return fmt.Errorf("store: migrate %d->%d: %w", cur, schemaVersion, err)
		}
		if cur < 31 {
			if _, err := d.w.Exec(`UPDATE model_rules SET win_tz='UTC' WHERE trim(win_tz)=''`); err != nil {
				return err
			}
		}

		if _, err := d.w.Exec(`INSERT INTO schema_version(version) VALUES(?)`, schemaVersion); err != nil {
			return fmt.Errorf("store: stamp version: %w", err)
		}
	}
	return nil
}

// renameWireOpenAIToChat (v30) renames the chat-completions wire from "openai"
// to "chat": the models.proto_openai column, the pins stored in
// models.proto_pin, and the surface recorded on every ledger row. Idempotent —
// each step is a no-op once applied, and a database that never had the old
// names is untouched.
func (d *DB) renameWireOpenAIToChat(ctx context.Context) error {
	if exists, err := hasTable(ctx, d.w, "models"); err != nil {
		return err
	} else if exists {
		old, err := hasColumn(ctx, d.w, "models", "proto_openai")
		if err != nil {
			return err
		}
		if has, err := hasColumn(ctx, d.w, "models", "proto_chat"); err != nil {
			return err
		} else if old && !has {
			if _, err := d.w.ExecContext(ctx, `ALTER TABLE models RENAME COLUMN proto_openai TO proto_chat`); err != nil {
				return err
			}
		}
		if _, err := d.w.ExecContext(ctx, `UPDATE models SET proto_pin='chat' WHERE proto_pin='openai'`); err != nil {
			return err
		}
	}
	if exists, err := hasTable(ctx, d.w, "calls"); err != nil {
		return err
	} else if exists {
		if _, err := d.w.ExecContext(ctx, `UPDATE calls SET surface='chat' WHERE surface='openai'`); err != nil {
			return err
		}
	}
	return nil
}

// alterAccountCatalogSynced adds accounts.catalog_synced_at on databases
// created before v27. Guarded by a column check: ALTER TABLE ADD COLUMN is
// not idempotent. Existing rows read as never synced until the next refresh.
func (d *DB) alterAccountCatalogSynced(ctx context.Context) error {
	exists, err := hasTable(ctx, d.w, "accounts")
	if err != nil || !exists {
		return err
	}
	has, err := hasColumn(ctx, d.w, "accounts", "catalog_synced_at")
	if err != nil || has {
		return err
	}
	_, err = d.w.ExecContext(ctx, `ALTER TABLE accounts ADD COLUMN catalog_synced_at TEXT`)
	return err
}

// alterAccountUsageConvention adds accounts.usage_convention on databases created
// before v13. Guarded by a column check: ALTER TABLE ADD COLUMN is not
// idempotent. Existing rows take the documented default 'auto' (unknown) —
// the value must be operator-declared, never back-filled by guess.
func (d *DB) alterAccountUsageConvention(ctx context.Context) error {
	exists, err := hasTable(ctx, d.w, "accounts")
	if err != nil || !exists {
		return err
	}
	if ok, err := hasColumn(ctx, d.w, "accounts", "usage_convention"); err != nil || ok {
		return err
	}
	_, err = d.w.ExecContext(ctx, `ALTER TABLE accounts ADD COLUMN usage_convention TEXT NOT NULL DEFAULT 'auto'`)
	return err
}

// alterAccountManagementKey adds accounts.management_key_plain on databases
// created before v19. Nullable: most providers have no management key.
func (d *DB) alterAccountManagementKey(ctx context.Context) error {
	exists, err := hasTable(ctx, d.w, "accounts")
	if err != nil || !exists {
		return err
	}
	if ok, err := hasColumn(ctx, d.w, "accounts", "management_key_plain"); err != nil || ok {
		return err
	}
	_, err = d.w.ExecContext(ctx, `ALTER TABLE accounts ADD COLUMN management_key_plain TEXT`)
	return err
}

// alterClientTokenPolicy adds allowed_models + expires_at on databases created
// before v21. Both default to empty (every model / never expires).
func (d *DB) alterClientTokenPolicy(ctx context.Context) error {
	exists, err := hasTable(ctx, d.w, "client_tokens")
	if err != nil || !exists {
		return err
	}
	if ok, err := hasColumn(ctx, d.w, "client_tokens", "allowed_models"); err != nil {
		return err
	} else if !ok {
		if _, err := d.w.ExecContext(ctx, `ALTER TABLE client_tokens ADD COLUMN allowed_models TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if ok, err := hasColumn(ctx, d.w, "client_tokens", "expires_at"); err != nil {
		return err
	} else if !ok {
		if _, err := d.w.ExecContext(ctx, `ALTER TABLE client_tokens ADD COLUMN expires_at TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	return nil
}

// alterModelPriceSource adds provenance columns to model_prices on databases
// created before v22. Existing rows keep source='manual' and no fetched_at.
func (d *DB) alterModelPriceSource(ctx context.Context) error {
	exists, err := hasTable(ctx, d.w, "model_prices")
	if err != nil || !exists {
		return err
	}
	if ok, err := hasColumn(ctx, d.w, "model_prices", "source"); err != nil {
		return err
	} else if !ok {
		if _, err := d.w.ExecContext(ctx, `ALTER TABLE model_prices ADD COLUMN source TEXT NOT NULL DEFAULT 'manual'`); err != nil {
			return err
		}
	}
	if ok, err := hasColumn(ctx, d.w, "model_prices", "fetched_at"); err != nil {
		return err
	} else if !ok {
		if _, err := d.w.ExecContext(ctx, `ALTER TABLE model_prices ADD COLUMN fetched_at TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	return nil
}

// alterProfileFreshToolOutput adds compression_profiles.fresh_tool_output on
// databases created before v24. Guarded by a column check: ALTER TABLE ADD
// COLUMN is not idempotent. Existing profiles take 'keep' — today's behaviour.
func (d *DB) alterProfileFreshToolOutput(ctx context.Context) error {
	exists, err := hasTable(ctx, d.w, "compression_profiles")
	if err != nil || !exists {
		return err
	}
	if ok, err := hasColumn(ctx, d.w, "compression_profiles", "fresh_tool_output"); err != nil || ok {
		return err
	}
	_, err = d.w.ExecContext(ctx, `ALTER TABLE compression_profiles ADD COLUMN fresh_tool_output TEXT NOT NULL DEFAULT 'keep'`)
	return err
}

// alterComboHopHold adds combo_hops.hold_until_failure on databases created
// before v14. Guarded by a column check: ALTER TABLE ADD COLUMN is not
// idempotent.
func (d *DB) alterComboHopHold(ctx context.Context) error {
	exists, err := hasTable(ctx, d.w, "combo_hops")
	if err != nil || !exists {
		return err
	}
	if ok, err := hasColumn(ctx, d.w, "combo_hops", "hold_until_failure"); err != nil || ok {
		return err
	}
	_, err = d.w.ExecContext(ctx, `ALTER TABLE combo_hops ADD COLUMN hold_until_failure INTEGER NOT NULL DEFAULT 0`)
	return err
}

// alterCallsIP adds calls.ip on databases created before v14.
func (d *DB) alterCallsIP(ctx context.Context) error {
	exists, err := hasTable(ctx, d.w, "calls")
	if err != nil || !exists {
		return err
	}
	if ok, err := hasColumn(ctx, d.w, "calls", "ip"); err != nil || ok {
		return err
	}
	_, err = d.w.ExecContext(ctx, `ALTER TABLE calls ADD COLUMN ip TEXT NOT NULL DEFAULT ''`)
	return err
}

// alterModelProtoPin adds models.proto_pin on databases created before v7.
// Guarded by a column check: ALTER TABLE ADD COLUMN is not idempotent.
func (d *DB) alterModelProtoPin(ctx context.Context) error {
	exists, err := hasTable(ctx, d.w, "models")
	if err != nil || !exists {
		return err
	}
	has, err := hasColumn(ctx, d.w, "models", "proto_pin")
	if err != nil || has {
		return err
	}
	_, err = d.w.ExecContext(ctx, `ALTER TABLE models ADD COLUMN proto_pin TEXT`)
	return err
}

// alterModelProtoSource adds models.proto_source / proto_note on databases
// created before v11 — provenance for operator-curated capability verdicts.
// Guarded by a column check: ALTER TABLE ADD COLUMN is not idempotent.
func (d *DB) alterModelProtoSource(ctx context.Context) error {
	exists, err := hasTable(ctx, d.w, "models")
	if err != nil || !exists {
		return err
	}
	has, err := hasColumn(ctx, d.w, "models", "proto_source")
	if err != nil || has {
		return err
	}
	if _, err = d.w.ExecContext(ctx, `ALTER TABLE models ADD COLUMN proto_source TEXT`); err != nil {
		return err
	}
	_, err = d.w.ExecContext(ctx, `ALTER TABLE models ADD COLUMN proto_note TEXT`)
	return err
}

// alterModelReasoning adds models.reasoning_vocab / reasoning_required on
// databases created before v29 — what a model accepts for `reasoning_effort`,
// learned from its own 400. NULL on a fresh row means "nothing known", which is
// the pre-learning behaviour: the clamp is inert until a rule exists.
// Guarded by a column check: ALTER TABLE ADD COLUMN is not idempotent.
func (d *DB) alterModelReasoning(ctx context.Context) error {
	exists, err := hasTable(ctx, d.w, "models")
	if err != nil || !exists {
		return err
	}
	has, err := hasColumn(ctx, d.w, "models", "reasoning_vocab")
	if err != nil || has {
		return err
	}
	if _, err = d.w.ExecContext(ctx, `ALTER TABLE models ADD COLUMN reasoning_vocab TEXT`); err != nil {
		return err
	}
	_, err = d.w.ExecContext(ctx, `ALTER TABLE models ADD COLUMN reasoning_required INTEGER`)
	return err
}

// alterAccountRate adds accounts.rate_limit / accounts.rate_window on databases
// created before v9. Guarded by a column check: ALTER TABLE ADD COLUMN is not
// idempotent, and fresh databases already get both from schema.sql.
func (d *DB) alterAccountRate(ctx context.Context) error {
	exists, err := hasTable(ctx, d.w, "accounts")
	if err != nil {
		return fmt.Errorf("check accounts table: %w", err)
	}
	if !exists {
		return nil
	}
	has, err := hasColumn(ctx, d.w, "accounts", "rate_limit")
	if err != nil || has {
		return err
	}
	if _, err := d.w.ExecContext(ctx, `ALTER TABLE accounts ADD COLUMN rate_limit INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	has, err = hasColumn(ctx, d.w, "accounts", "rate_window")
	if err != nil || has {
		return err
	}
	_, err = d.w.ExecContext(ctx, `ALTER TABLE accounts ADD COLUMN rate_window TEXT NOT NULL DEFAULT ''`)
	return err
}

// rebuildCombosV9 gives `combos` its block_size column and drops the duplicated
// strategy CHECK constraint.
//
// This is a full table REBUILD, not an ALTER, and that is the whole point:
// SQLite cannot alter or drop a CHECK constraint, so adding a single strategy
// value ('block_round_robin') to the old CHECK would otherwise be impossible
// without rebuilding. Doing both in one rebuild means the next strategy costs
// no migration at all, because validation lives in Go (store.ValidStrategies).
//
// Guarded by the block_size column: a fresh database gets the new shape straight
// from schema.sql and a migrated one already has it, so this is a no-op for both.
// Row ids are preserved, which is load-bearing — combo_hops.combo_id references
// them. The deferred foreign key means we must toggle enforcement off around the
// DROP/RENAME and verify with foreign_key_check afterwards.
func (d *DB) rebuildCombosV9(ctx context.Context) (retErr error) {
	exists, err := hasTable(ctx, d.w, "combos")
	if err != nil {
		return err
	}
	staged, err := hasTable(ctx, d.w, "combos_v9")
	if err != nil {
		return err
	}
	if !exists && !staged {
		return nil
	}
	current := false
	if exists {
		current, err = hasColumn(ctx, d.w, "combos", "block_size")
		if err != nil {
			return err
		}
	}
	if current && !staged {
		return nil
	}
	if current && staged {
		return fmt.Errorf("combos rebuild: both current and staged tables exist; preserve both for manual reconciliation")
	}
	conn, err := d.w.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return err
	}
	defer func() {
		_, err := conn.ExecContext(context.Background(), `PRAGMA foreign_keys=ON`)
		retErr = errors.Join(retErr, err)
	}()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if staged && !exists {
		// The old process dropped the original but did not rename its copy.
		if _, err = tx.ExecContext(ctx, `ALTER TABLE combos_v9 RENAME TO combos`); err != nil {
			return err
		}
	} else if staged {
		// Original is authoritative until DROP. Discard only the incomplete copy.
		if _, err = tx.ExecContext(ctx, `DROP TABLE combos_v9`); err != nil {
			return err
		}
	}
	hasBlock, err := hasColumn(ctx, tx, "combos", "block_size")
	if err != nil {
		return err
	}
	if !hasBlock {
		contextExpr := "200000"
		hasContext, err := hasColumn(ctx, tx, "combos", "context_size")
		if err != nil {
			return err
		}
		if hasContext {
			contextExpr = "COALESCE(context_size, 200000)"
		}
		if _, err = tx.ExecContext(ctx, `CREATE TABLE combos_v9 (
  id          INTEGER PRIMARY KEY,
  name        TEXT NOT NULL UNIQUE,
  enabled     INTEGER NOT NULL DEFAULT 1,
  strategy    TEXT NOT NULL DEFAULT 'failover',
  sticky_idle_s INTEGER NOT NULL DEFAULT 1800,
  compression_profile_id INTEGER REFERENCES compression_profiles(id),
  context_size INTEGER NOT NULL DEFAULT 200000 CHECK (context_size BETWEEN 1 AND 10000000),
  block_size  INTEGER NOT NULL DEFAULT 1 CHECK (block_size BETWEEN 1 AND 1000),
  rr_cursor   INTEGER NOT NULL DEFAULT 0,
  rr_epoch    INTEGER NOT NULL DEFAULT 0,
  notes       TEXT,
  created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
)`); err != nil {
			return fmt.Errorf("combos rebuild: create: %w", err)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO combos_v9 (id,name,enabled,strategy,sticky_idle_s,compression_profile_id,context_size,block_size,rr_cursor,rr_epoch,notes,created_at)
SELECT id,name,enabled,strategy,sticky_idle_s,compression_profile_id,`+contextExpr+`,1,0,0,notes,created_at FROM combos`); err != nil {
			return fmt.Errorf("combos rebuild: copy: %w", err)
		}
		if _, err = tx.ExecContext(ctx, `DROP TABLE combos`); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `ALTER TABLE combos_v9 RENAME TO combos`); err != nil {
			return err
		}
	}
	hasHops, err := hasTable(ctx, tx, "combo_hops")
	if err != nil {
		return err
	}
	if hasHops {
		var orphans int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM combo_hops h LEFT JOIN combos c ON c.id=h.combo_id WHERE c.id IS NULL`).Scan(&orphans); err != nil {
			return err
		}
		if orphans != 0 {
			return fmt.Errorf("combos rebuild: %d combo_hops rows lost their combo", orphans)
		}
	}
	return tx.Commit()
}

// alterCallsResearch adds the v8 (M9) research columns to `calls` on databases
// created before them. Must run BEFORE schema.sql re-applies, because schema.sql
// creates idx_calls_prefix/idx_calls_session over prefix_sha/session_id — on a
// pre-v8 table CREATE TABLE IF NOT EXISTS is a no-op (no such column) while the
// CREATE INDEX still runs and aborts boot. Same guard discipline as the others:
// ALTER TABLE ADD COLUMN is not idempotent; fresh databases get them from
// schema.sql directly. TEXT columns default ”; counters default 0.
func (d *DB) alterCallsResearch(ctx context.Context) error {
	exists, err := hasTable(ctx, d.w, "calls")
	if err != nil {
		return fmt.Errorf("check calls table: %w", err)
	}
	if !exists {
		return nil
	}
	textCols := []string{"prefix_sha", "session_id"}
	intCols := []string{"compression_saved_notional", "msg_count", "tool_count", "req_bytes", "block_pos"}
	for _, column := range textCols {
		ok, err := hasColumn(ctx, d.w, "calls", column)
		if err != nil {
			return fmt.Errorf("check calls.%s: %w", column, err)
		}
		if ok {
			continue
		}
		if _, err := d.w.ExecContext(ctx, fmt.Sprintf("ALTER TABLE calls ADD COLUMN %s TEXT NOT NULL DEFAULT ''", column)); err != nil {
			return fmt.Errorf("add calls.%s: %w", column, err)
		}
	}
	for _, column := range intCols {
		ok, err := hasColumn(ctx, d.w, "calls", column)
		if err != nil {
			return fmt.Errorf("check calls.%s: %w", column, err)
		}
		if ok {
			continue
		}
		if _, err := d.w.ExecContext(ctx, fmt.Sprintf("ALTER TABLE calls ADD COLUMN %s INTEGER NOT NULL DEFAULT 0", column)); err != nil {
			return fmt.Errorf("add calls.%s: %w", column, err)
		}
	}
	return nil
}

// alterCallsTranslated adds calls.translated (schema v12) on databases created
// before it. Attribution for wire-translated hops (anthropic client ->
// openai-only hop): the marker carries two semantics with it — tokens_in is the
// vendor's INCLUSIVE total (the synthesized anthropic envelope passes OpenAI's
// prompt_tokens) and tokens_cached_read stays 0. Guarded by a column check:
// ALTER TABLE ADD COLUMN is not idempotent.
// alterCallsCompressionVersion adds calls.compression_version (addendum #5:
// the immutable profile fingerprint behind each accepted transformation).
func (d *DB) alterCallsCompressionVersion(ctx context.Context) error {
	exists, err := hasTable(ctx, d.w, "calls")
	if err != nil {
		return fmt.Errorf("check calls table: %w", err)
	}
	if !exists {
		return nil
	}
	ok, err := hasColumn(ctx, d.w, "calls", "compression_version")
	if err != nil {
		return fmt.Errorf("check calls.compression_version: %w", err)
	}
	if ok {
		return nil
	}
	if _, err := d.w.ExecContext(ctx,
		`ALTER TABLE calls ADD COLUMN compression_version TEXT`); err != nil {
		return fmt.Errorf("alter calls compression_version: %w", err)
	}
	return nil
}

// alterCallsCompressionAttribution adds calls.compression_stages +
// calls.shadow_tokens_saved (addendum #7): per-engine attribution and the
// sampled shadow measurement.
// alterAccountMaxContext adds accounts.max_context_tokens (addendum #2/#6).
func (d *DB) alterAccountMaxContext(ctx context.Context) error {
	exists, err := hasTable(ctx, d.w, "accounts")
	if err != nil {
		return fmt.Errorf("check accounts table: %w", err)
	}
	if !exists {
		return nil
	}
	ok, err := hasColumn(ctx, d.w, "accounts", "max_context_tokens")
	if err != nil {
		return fmt.Errorf("check accounts.max_context_tokens: %w", err)
	}
	if ok {
		return nil
	}
	if _, err := d.w.ExecContext(ctx,
		`ALTER TABLE accounts ADD COLUMN max_context_tokens INTEGER NOT NULL DEFAULT 0`); err != nil {
		return fmt.Errorf("alter accounts max_context_tokens: %w", err)
	}
	return nil
}

func (d *DB) alterCallsCompressionAttribution(ctx context.Context) error {
	exists, err := hasTable(ctx, d.w, "calls")
	if err != nil {
		return fmt.Errorf("check calls table: %w", err)
	}
	if !exists {
		return nil
	}
	for _, col := range []struct {
		name, ddl string
	}{
		{"compression_stages", `ALTER TABLE calls ADD COLUMN compression_stages TEXT`},
		{"shadow_tokens_saved", `ALTER TABLE calls ADD COLUMN shadow_tokens_saved INTEGER NOT NULL DEFAULT 0`},
	} {
		ok, err := hasColumn(ctx, d.w, "calls", col.name)
		if err != nil {
			return fmt.Errorf("check calls.%s: %w", col.name, err)
		}
		if ok {
			continue
		}
		if _, err := d.w.ExecContext(ctx, col.ddl); err != nil {
			return fmt.Errorf("alter calls %s: %w", col.name, err)
		}
	}
	return nil
}

func (d *DB) alterCallsTranslated(ctx context.Context) error {
	exists, err := hasTable(ctx, d.w, "calls")
	if err != nil {
		return fmt.Errorf("check calls table: %w", err)
	}
	if !exists {
		return nil
	}
	ok, err := hasColumn(ctx, d.w, "calls", "translated")
	if err != nil {
		return fmt.Errorf("check calls.translated: %w", err)
	}
	if ok {
		return nil
	}
	if _, err := d.w.ExecContext(ctx,
		`ALTER TABLE calls ADD COLUMN translated INTEGER NOT NULL DEFAULT 0`); err != nil {
		return fmt.Errorf("add calls.translated: %w", err)
	}
	return nil
}

// alterComboContextSize adds the client-advertised context metadata on existing DBs.
func (d *DB) alterComboContextSize(ctx context.Context) error {
	exists, err := hasTable(ctx, d.w, "combos")
	if err != nil || !exists {
		return err
	}
	has, err := hasColumn(ctx, d.w, "combos", "context_size")
	if err != nil || has {
		return err
	}
	_, err = d.w.ExecContext(ctx, `ALTER TABLE combos ADD COLUMN context_size INTEGER NOT NULL DEFAULT 200000`)
	return err
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

// alterCallsRulesFired adds calls.compression_rules_fired (v3, caveman
// attribution) on databases created before it. Same guard as
// alterCallsAccountID: ALTER TABLE ADD COLUMN is not idempotent, and a fresh
// database already has the column from schema.sql.
func (d *DB) alterCallsContextTokens(ctx context.Context) error {
	exists, err := hasTable(ctx, d.w, "calls")
	if err != nil {
		return fmt.Errorf("check calls table: %w", err)
	}
	if !exists {
		return nil
	}
	for _, column := range []string{"context_tokens_pre", "context_tokens_saved"} {
		ok, err := hasColumn(ctx, d.w, "calls", column)
		if err != nil {
			return fmt.Errorf("check calls.%s: %w", column, err)
		}
		if ok {
			continue
		}
		if _, err := d.w.ExecContext(ctx, fmt.Sprintf("ALTER TABLE calls ADD COLUMN %s INTEGER NOT NULL DEFAULT 0", column)); err != nil {
			return fmt.Errorf("add calls.%s: %w", column, err)
		}
	}
	return nil
}

// schema.sql is re-applied after migrations, so these CREATE statements are
// idempotent and keep the standalone version reset straightforward.
func (d *DB) alterDashboardAuth(ctx context.Context) error {
	_, err := d.w.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS dashboard_auth (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    password_hash TEXT NOT NULL,
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE TABLE IF NOT EXISTS dashboard_sessions (
    token_hash TEXT PRIMARY KEY,
    expires_at TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE INDEX IF NOT EXISTS idx_dashboard_sessions_expiry ON dashboard_sessions(expires_at);`)
	if err != nil {
		return err
	}
	// v26: first_boot. Existing rows default to 0 (password set);
	// EnsureDashboardPassword re-flags a row still on the legacy factory
	// password.
	exists, err := hasColumn(ctx, d.w, "dashboard_auth", "first_boot")
	if err != nil || exists {
		return err
	}
	_, err = d.w.ExecContext(ctx, `ALTER TABLE dashboard_auth ADD COLUMN first_boot INTEGER NOT NULL DEFAULT 0`)
	return err
}

func (d *DB) alterCallsRulesFired(ctx context.Context) error {
	exists, err := hasTable(ctx, d.w, "calls")
	if err != nil {
		return fmt.Errorf("check calls table: %w", err)
	}
	if !exists {
		return nil
	}
	ok, err := hasColumn(ctx, d.w, "calls", "compression_rules_fired")
	if err != nil {
		return fmt.Errorf("check calls.compression_rules_fired: %w", err)
	}
	if ok {
		return nil
	}
	if _, err := d.w.ExecContext(ctx,
		`ALTER TABLE calls ADD COLUMN compression_rules_fired INTEGER NOT NULL DEFAULT 0`); err != nil {
		return fmt.Errorf("add calls.compression_rules_fired: %w", err)
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
	d.recMu.Lock()
	if !d.closed {
		d.closed = true
		close(d.closeCh)
	}
	d.recMu.Unlock()
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
