package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestRecoveryInterruptedComboRebuild(t *testing.T) {
	for _, dropped := range []bool{false, true} {
		t.Run(map[bool]string{false: "before_drop", true: "after_drop"}[dropped], func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "interrupted.sqlite")
			db, err := Open(ctx, Options{Path: path})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.w.Exec(`PRAGMA foreign_keys=OFF; DROP TABLE combos`); err != nil {
				t.Fatal(err)
			}
			if _, err = db.w.Exec(oldCombosDDL); err != nil {
				t.Fatal(err)
			}
			if _, err = db.w.Exec(`INSERT INTO combos(id,name,strategy) VALUES(7,'survivor','failover'); CREATE TABLE combos_v9 AS SELECT * FROM combos; UPDATE schema_version SET version=8`); err != nil {
				t.Fatal(err)
			}
			if dropped {
				if _, err = db.w.Exec(`DROP TABLE combos`); err != nil {
					t.Fatal(err)
				}
			}
			if err = db.Close(); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				db, err = Open(ctx, Options{Path: path})
				if err != nil {
					t.Fatal(err)
				}
				var id int
				var fk int
				if err = db.r.QueryRow(`SELECT id FROM combos WHERE name='survivor'`).Scan(&id); err != nil || id != 7 {
					t.Fatalf("combo lost: %d %v", id, err)
				}
				if err = db.w.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
					t.Fatalf("foreign keys not restored: %d %v", fk, err)
				}
				if exists, err := hasTable(ctx, db.w, "combos_v9"); err != nil || exists {
					t.Fatalf("staging remains: %t %v", exists, err)
				}
				if err = db.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestRecoveryComboRebuildRollsBackOnFailure(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	if _, err := db.w.Exec(`PRAGMA foreign_keys=OFF; DROP TABLE combos`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.w.Exec(oldCombosDDL); err != nil {
		t.Fatal(err)
	}
	// Invalid legacy context fails the new table's CHECK after staging creation.
	if _, err := db.w.Exec(`ALTER TABLE combos ADD COLUMN context_size INTEGER; INSERT INTO combos(id,name,context_size) VALUES(7,'survivor',-1); PRAGMA foreign_keys=ON`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := db.rebuildCombosV9(ctx); err == nil {
			t.Fatal("invalid context accepted")
		}
		var n, fk int
		if err := db.r.QueryRow(`SELECT COUNT(*) FROM combos WHERE name='survivor'`).Scan(&n); err != nil || n != 1 {
			t.Fatalf("original lost: %d %v", n, err)
		}
		if exists, err := hasTable(ctx, db.w, "combos_v9"); err != nil || exists {
			t.Fatalf("failed transaction left staging: %t %v", exists, err)
		}
		if err := db.w.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
			t.Fatalf("FK state %d %v", fk, err)
		}
	}
}

func TestRecoveryAmbiguousComboStagingIsPreserved(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	if _, err := db.w.Exec(`CREATE TABLE combos_v9 AS SELECT * FROM combos`); err != nil {
		t.Fatal(err)
	}
	if err := db.rebuildCombosV9(ctx); err == nil {
		t.Fatal("ambiguous current and staged tables accepted")
	}
	for _, name := range []string{"combos", "combos_v9"} {
		if exists, err := hasTable(ctx, db.w, name); err != nil || !exists {
			t.Fatalf("evidence lost %s: %t %v", name, exists, err)
		}
	}
}
