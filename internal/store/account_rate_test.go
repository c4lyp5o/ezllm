package store

// v9 migration + per-account request budget.
//
// The migration test exercises the REAL upgrade branch rather than a fresh
// database: fresh databases get accounts.rate_limit/rate_window straight from
// schema.sql, so a bug in the ALTER path would be invisible to them — exactly
// the trap the v1→v2 migration test was written for. Here the columns are
// dropped and the version stamped back to 8 to reproduce what a deployed
// database actually looks like, then Open() is made to migrate it.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMigrateV8ToV9AddsAccountRateColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v9.sqlite")
	ctx := context.Background()

	db, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// An account that must survive the migration untouched.
	if _, err := db.CreateAccount(ctx, AccountInput{
		Name: "Keeper", Namespace: "keeper", Kind: "openai-compatible",
		BaseURL: "https://example.invalid/v1",
	}); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	// Reproduce a v8 database: no rate columns, version 8.
	for _, col := range []string{"rate_limit", "rate_window"} {
		if _, err := db.w.ExecContext(ctx, "ALTER TABLE accounts DROP COLUMN "+col); err != nil {
			t.Fatalf("drop %s (SQLite >= 3.35 required): %v", col, err)
		}
	}
	if _, err := db.w.ExecContext(ctx, `UPDATE schema_version SET version = 8`); err != nil {
		t.Fatalf("stamp v8: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen → cur(8) < schemaVersion → the upgrade branch must run.
	migrated, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("reopen/migrate: %v", err)
	}
	defer migrated.Close()

	// The claim is "this DB was migrated UP to the binary's version" — assert
	// the constant, not a literal, so the next schema bump updates one place.
	if got := migrated.SchemaVersion(); got != schemaVersion {
		t.Fatalf("schema version = %d, want %d", got, schemaVersion)
	}
	// v10 rides the same upgrade: calls.block_pos must exist on a pre-v9 DB
	// after the idempotent ALTER loop (this is the migration the dashboard's
	// block-position tag depends on).
	if has, err := hasColumn(ctx, migrated.w, "calls", "block_pos"); err != nil || !has {
		t.Fatalf("calls.block_pos missing after migrate (has=%v err=%v)", has, err)
	}
	for _, col := range []string{"rate_limit", "rate_window"} {
		has, err := hasColumn(ctx, migrated.w, "accounts", col)
		if err != nil {
			t.Fatalf("hasColumn %s: %v", col, err)
		}
		if !has {
			t.Fatalf("accounts.%s was not added by the v8→v9 migration", col)
		}
	}
	// The existing account survived, and defaults to unlimited.
	acct, err := migrated.AccountByNamespace(ctx, "keeper")
	if err != nil {
		t.Fatalf("existing account lost in migration: %v", err)
	}
	if acct.RateLimit != 0 || acct.RateWindow != 0 {
		t.Errorf("migrated account got a budget it never had: %d/%v", acct.RateLimit, acct.RateWindow)
	}
	// Re-running must be a no-op, not an error: ALTER TABLE ADD COLUMN is not
	// idempotent, which is exactly why it is guarded by a column check.
	if err := migrated.alterAccountRate(ctx); err != nil {
		t.Fatalf("second alter should be a no-op, got %v", err)
	}
}

func TestAccountRateRoundTrip(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()

	limit := int64(5)
	id, err := db.CreateAccount(ctx, AccountInput{
		Name: "Router", Namespace: "tr", Kind: "openai-compatible",
		BaseURL:   "https://example.invalid/v1",
		RateLimit: &limit, RateWindow: "60s",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// The runtime view the router and limiter actually read.
	acct, err := db.AccountByID(ctx, id)
	if err != nil {
		t.Fatalf("AccountByID: %v", err)
	}
	if acct.RateLimit != 5 || acct.RateWindow != time.Minute {
		t.Fatalf("runtime account = %d/%v, want 5/1m", acct.RateLimit, acct.RateWindow)
	}
	// The admin view the dashboard reads.
	summaries, err := db.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	var found bool
	for _, s := range summaries {
		if s.ID == id {
			found = true
			if s.RateLimit != 5 || s.RateWindow != "60s" {
				t.Errorf("summary = %d/%q, want 5/60s", s.RateLimit, s.RateWindow)
			}
		}
	}
	if !found {
		t.Fatal("created account missing from ListAccounts")
	}

	// A PATCH that omits the budget must leave it alone.
	if err := db.UpdateAccount(ctx, id, AccountInput{Notes: "touched"}); err != nil {
		t.Fatalf("patch notes: %v", err)
	}
	acct, _ = db.AccountByID(ctx, id)
	if acct.RateLimit != 5 || acct.RateWindow != time.Minute {
		t.Errorf("omitted budget was clobbered: %d/%v", acct.RateLimit, acct.RateWindow)
	}

	// An explicit 0 clears it — and must take the window with it, or the row
	// would read as "configured" with no count.
	zero := int64(0)
	if err := db.UpdateAccount(ctx, id, AccountInput{RateLimit: &zero}); err != nil {
		t.Fatalf("clear budget: %v", err)
	}
	acct, _ = db.AccountByID(ctx, id)
	if acct.RateLimit != 0 || acct.RateWindow != 0 {
		t.Errorf("cleared budget = %d/%v, want 0/0", acct.RateLimit, acct.RateWindow)
	}
}

func TestAccountRateValidation(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	l := func(v int64) *int64 { return &v }

	cases := []struct {
		name    string
		limit   *int64
		window  string
		wantErr bool
	}{
		{"both set", l(5), "60s", false},
		{"unlimited", l(0), "", false},
		{"neither", nil, "", false},
		// A count with no window has no meaning, and a window with no count is a
		// limit of zero that would refuse every request — both are refused at
		// write time so neither can become a silently dead account.
		{"count without window", l(5), "", true},
		// A window with a zero count is harmless, not an error: 0 means
		// unlimited, so the window is simply inert (and writeAccount clears it).
		{"window without count", l(0), "60s", false},
		{"negative count", l(-1), "60s", true},
		{"unparseable window", l(5), "a minute", true},
		{"window too short", l(5), "500ms", true},
		{"window too long", l(5), "48h", true},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Namespaces are slugs; the table names contain spaces.
			slug := fmt.Sprintf("ns-%d", i)
			_, err := db.CreateAccount(ctx, AccountInput{
				Name: "n-" + slug, Namespace: slug,
				Kind: "openai-compatible", BaseURL: "https://example.invalid/v1",
				RateLimit: tc.limit, RateWindow: tc.window,
			})
			if tc.wantErr && err == nil {
				t.Fatal("expected a validation error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestAccountsWithRateLimitAndCallTimesSeed(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	l := func(v int64) *int64 { return &v }

	budgeted, err := db.CreateAccount(ctx, AccountInput{
		Name: "Budgeted", Namespace: "budgeted", Kind: "openai-compatible",
		BaseURL: "https://example.invalid/v1", RateLimit: l(2), RateWindow: "60s",
	})
	if err != nil {
		t.Fatalf("create budgeted: %v", err)
	}
	free, err := db.CreateAccount(ctx, AccountInput{
		Name: "Free", Namespace: "free", Kind: "openai-compatible",
		BaseURL: "https://example.invalid/v1",
	})
	if err != nil {
		t.Fatalf("create free: %v", err)
	}

	rates, err := db.AccountsWithRateLimit(ctx)
	if err != nil {
		t.Fatalf("AccountsWithRateLimit: %v", err)
	}
	if len(rates) != 1 || rates[0].AccountID != budgeted {
		t.Fatalf("rates = %+v, want exactly the budgeted account %d", rates, budgeted)
	}
	if rates[0].Limit != 2 || rates[0].Window != time.Minute {
		t.Errorf("parsed rate = %d/%v, want 2/1m", rates[0].Limit, rates[0].Window)
	}

	// Two calls inside the window and one outside it: only the recent pair may
	// come back, because those are the ones that still count against the budget.
	now := time.Now().UTC()
	for _, c := range []struct {
		acct int64
		ago  time.Duration
	}{
		{budgeted, 50 * time.Second},
		{budgeted, 10 * time.Second},
		{budgeted, 30 * time.Minute},
		{free, 10 * time.Second},
	} {
		if _, err := db.w.ExecContext(ctx,
			`INSERT INTO calls (ts, client, surface, account_id, account, model, status) VALUES (?,?,?,?,?,?,?)`,
			now.Add(-c.ago).Format(ledgerTSLayout), "t", "chat", c.acct, "ns", "m", 200); err != nil {
			t.Fatalf("insert call: %v", err)
		}
	}

	times, err := db.CallTimesSince(ctx, budgeted, now.Add(-time.Minute))
	if err != nil {
		t.Fatalf("CallTimesSince: %v", err)
	}
	if len(times) != 2 {
		t.Fatalf("seed times = %d, want 2 (the out-of-window call must not count)", len(times))
	}
	if !times[0].Before(times[1]) {
		t.Errorf("seed times not oldest-first: %v", times)
	}
}

// TestRehearseMigrationOnLiveCopy is a MANUAL gate, skipped unless
// EZLLM_REHEARSE_DB points at a COPY of the production database. It runs the
// real upgrade path in place and proves the ledger, accounts and version all
// come through intact — the rehearsal that has to happen before any deploy
// carrying a schema bump.
//
// Run:
//
//	cp data/ezllm.sqlite /tmp/rehearse.sqlite
//	EZLLM_REHEARSE_DB=/tmp/rehearse.sqlite go test ./internal/store/ -run Rehearse -v
func TestRehearseMigrationOnLiveCopy(t *testing.T) {
	path := os.Getenv("EZLLM_REHEARSE_DB")
	if path == "" {
		t.Skip("set EZLLM_REHEARSE_DB to a COPY of the live database to run this")
	}
	ctx := context.Background()

	// Read the starting state with the RAW driver, so the measurement itself
	// cannot trigger the migration it is measuring.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	count := func(db *sql.DB, q string) int {
		var n int
		if err := db.QueryRow(q).Scan(&n); err != nil {
			t.Fatalf("count %q: %v", q, err)
		}
		return n
	}
	before := map[string]int{
		"calls":      count(raw, `SELECT COUNT(*) FROM calls`),
		"accounts":   count(raw, `SELECT COUNT(*) FROM accounts`),
		"combos":     count(raw, `SELECT COUNT(*) FROM combos`),
		"combo_hops": count(raw, `SELECT COUNT(*) FROM combo_hops`),
	}
	// The combos REBUILD drops and recreates the table, so identity and the
	// stored strategy are the things that must survive it.
	type comboRow struct {
		id       int64
		name     string
		strategy string
		sticky   int
	}
	readCombos := func(db *sql.DB) []comboRow {
		rows, err := db.Query(`SELECT id, name, strategy, sticky_idle_s FROM combos ORDER BY id`)
		if err != nil {
			t.Fatalf("read combos: %v", err)
		}
		defer rows.Close()
		var out []comboRow
		for rows.Next() {
			var c comboRow
			if err := rows.Scan(&c.id, &c.name, &c.strategy, &c.sticky); err != nil {
				t.Fatalf("scan combo: %v", err)
			}
			out = append(out, c)
		}
		return out
	}
	combosBefore := readCombos(raw)
	var startVersion int
	if err := raw.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&startVersion); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}
	t.Logf("BEFORE: version=%d calls=%d accounts=%d combos=%d hops=%d strategies=%v",
		startVersion, before["calls"], before["accounts"], before["combos"], before["combo_hops"], combosBefore)

	db, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	defer db.Close()

	after := map[string]int{
		"calls":      count(db.r, `SELECT COUNT(*) FROM calls`),
		"accounts":   count(db.r, `SELECT COUNT(*) FROM accounts`),
		"combos":     count(db.r, `SELECT COUNT(*) FROM combos`),
		"combo_hops": count(db.r, `SELECT COUNT(*) FROM combo_hops`),
	}
	combosAfter := readCombos(db.r)
	if len(combosBefore) != len(combosAfter) {
		t.Fatalf("combos survived %d -> %d", len(combosBefore), len(combosAfter))
	}
	for i := range combosBefore {
		if combosBefore[i] != combosAfter[i] {
			t.Errorf("combo changed across the rebuild: %+v -> %+v", combosBefore[i], combosAfter[i])
		}
	}
	t.Logf("AFTER:  version=%d calls=%d accounts=%d combos=%d hops=%d strategies=%v",
		db.SchemaVersion(), after["calls"], after["accounts"], after["combos"], after["combo_hops"], combosAfter)

	if db.SchemaVersion() != schemaVersion {
		t.Errorf("version = %d, want %d", db.SchemaVersion(), schemaVersion)
	}
	for table, want := range before {
		if got := after[table]; got != want {
			t.Errorf("%s rows = %d, want %d — the migration lost data", table, got, want)
		}
	}
	for _, col := range []string{"rate_limit", "rate_window"} {
		has, err := hasColumn(ctx, db.w, "accounts", col)
		if err != nil || !has {
			t.Errorf("accounts.%s missing after migration (has=%v err=%v)", col, has, err)
		}
	}
	// v14: the hold checkbox and the request IP land as guarded ALTERs, so a
	// pre-v14 database must gain both columns with safe defaults.
	for _, chk := range []struct{ table, col string }{
		{"combo_hops", "hold_until_failure"}, {"calls", "ip"},
	} {
		has, err := hasColumn(ctx, db.w, chk.table, chk.col)
		if err != nil || !has {
			t.Errorf("%s.%s missing after migration (has=%v err=%v)", chk.table, chk.col, has, err)
		}
	}
	if len(combosAfter) > 0 {
		var holds int
		if err := db.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM combo_hops WHERE hold_until_failure != 0`).Scan(&holds); err != nil {
			t.Errorf("hold_until_failure unreadable after migration: %v", err)
		} else if holds != 0 {
			t.Errorf("%d migrated hops carry a hold — a migration must never invent operator intent", holds)
		}
	}
	var emptyIPs int
	if err := db.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM calls WHERE ip != ''`).Scan(&emptyIPs); err != nil {
		t.Errorf("calls.ip unreadable after migration: %v", err)
	} else if emptyIPs != 0 {
		t.Errorf("%d migrated rows carry an ip — a migration must never invent captured data", emptyIPs)
	}
	if len(combosAfter) > 0 {
		has, err := hasColumn(ctx, db.w, "combos", "block_size")
		if err != nil || !has {
			t.Errorf("combos.block_size missing after migration (has=%v err=%v)", has, err)
		}
		// The old CHECK must be gone, or the new strategy is unstorable.
		var blockSize int
		if err := db.r.QueryRowContext(ctx, `SELECT block_size FROM combos ORDER BY id LIMIT 1`).Scan(&blockSize); err != nil {
			t.Errorf("block_size unreadable after migration: %v", err)
		} else if blockSize != 1 {
			t.Errorf("existing combo block_size = %d, want 1", blockSize)
		}
		if _, err := db.w.ExecContext(ctx, `UPDATE combos SET strategy='block_round_robin' WHERE id=(SELECT MIN(id) FROM combos)`); err != nil {
			t.Errorf("old CHECK still enforced on the live copy: %v", err)
		}
	}
	// Every account must still be readable through the runtime path the router
	// uses, with the budget defaulted rather than corrupt.
	rows, err := db.r.QueryContext(ctx, `SELECT namespace FROM accounts ORDER BY id`)
	if err != nil {
		t.Fatalf("list namespaces: %v", err)
	}
	defer rows.Close()
	var ns []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ns = append(ns, s)
	}
	for _, n := range ns {
		if _, err := db.AccountByNamespace(ctx, n); err != nil {
			t.Errorf("AccountByNamespace(%q) after migration: %v", n, err)
		}
	}
	t.Logf("all %d accounts still resolve through the router path", len(ns))
}
