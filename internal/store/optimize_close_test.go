package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// Close refreshes the planner's statistics (PRAGMA optimize), so a database
// whose indexed queries ran has sqlite_stat1 rows after a restart.
func TestCloseRunsOptimize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ezllm.sqlite")
	d, err := Open(context.Background(), Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 300; i++ {
		if _, err := d.w.ExecContext(ctx, `INSERT INTO calls (ts, client, surface, account, model, status) VALUES (?, ?, 'chat', 'a', 'm', 200)`, time.Now().Add(time.Duration(i)*time.Second).UTC().Format(time.RFC3339), "c"+string(rune('a'+i%5))); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := d.w.QueryRowContext(ctx, `SELECT COUNT(*) FROM calls WHERE client = 'ca' AND ts > ''`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var rows int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM sqlite_stat1`).Scan(&rows); err != nil || rows == 0 {
		t.Fatalf("sqlite_stat1 rows = %d (%v), want statistics after Close", rows, err)
	}
}
