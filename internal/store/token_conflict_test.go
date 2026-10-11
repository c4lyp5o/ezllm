package store

// C2 regression: UpsertToken's pre-check (SELECT name) is check-then-act, so a
// genuine race — two concurrent POST /admin/tokens with the same name — lands
// on the raw INSERT. SQLite's UNIQUE violation was returned unwrapped, so the
// server answered 500 instead of the 409 the pre-check itself would have
// produced, and which its siblings (CreateAccount, writeAccount) already map.
//
// The race window is untestable in-process directly, so this test reproduces
// the same SQL shape against the same schema: an INSERT that collides on
// client_tokens.name must yield a UNIQUE violation that isUniqueViolation
// recognises — the exact error the fix maps to *ErrConflict.

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// The contract the fix relies on, asserted directly: a duplicate-name INSERT
// raises an error isUniqueViolation classifies as a conflict.
func TestDuplicateTokenNameInsertIsAUniqueViolation(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO client_tokens (name, token_hash, roles, enabled) VALUES (?,?,?,1)`,
			"tok", "hash1", "infer")
		return err
	}); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	var dupErr error
	err := db.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, dupErr = tx.ExecContext(ctx,
			`INSERT INTO client_tokens (name, token_hash, roles, enabled) VALUES (?,?,?,1)`,
			"tok", "hash2", "infer")
		return dupErr
	})
	if err == nil {
		t.Fatal("duplicate-name insert unexpectedly succeeded")
	}
	if !isUniqueViolation(err) {
		t.Fatalf("duplicate-name insert error %q is not recognised as a UNIQUE violation — the fix's classification would miss it", err)
	}
}

// End-to-end through the real entry point: the same name submitted twice must
// answer *ErrConflict (409 at the server), never a raw SQLite error.
func TestUpsertTokenDuplicateNameIsErrConflict(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if _, _, err := db.UpsertToken(ctx, TokenInput{Name: "dup", Roles: []string{"infer"}}); err != nil {
		t.Fatalf("first UpsertToken: %v", err)
	}
	_, _, err := db.UpsertToken(ctx, TokenInput{Name: "dup", Roles: []string{"infer"}})
	var ec *ErrConflict
	if !errors.As(err, &ec) {
		t.Fatalf("second UpsertToken for the same name: want *ErrConflict (the server maps it to 409), got %T: %v", err, err)
	}
}
