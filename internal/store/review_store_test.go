package store

import (
	"context"
	"errors"
	"testing"
)

// Deleting the newest account and adding another must not hand the new one
// the old id, which the ledger and usage counters still use.
func TestAccountIDIsNeverReused(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	in := func(n string) AccountInput {
		return AccountInput{Name: n, Namespace: n, Kind: "openai-compatible", BaseURL: "https://api.example.com/v1"}
	}
	old, err := d.CreateAccount(ctx, in("old"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.w.ExecContext(ctx, `INSERT INTO calls (ts, surface, account, account_id, model, status)
		VALUES ('2026-01-01T00:00:00Z', 'chat', 'old', ?, 'm', 200)`, old); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteAccount(ctx, old, true); err != nil {
		t.Fatal(err)
	}
	fresh, err := d.CreateAccount(ctx, in("fresh"))
	if err != nil {
		t.Fatal(err)
	}
	if fresh == old {
		t.Fatalf("new account reused id %d", old)
	}
}

// A negative count from an upstream never subtracts from usage.
func TestNegativeUsageReadsAsZero(t *testing.T) {
	u := NormalizeUsageWith(SurfaceChat, map[string]any{
		"prompt_tokens": float64(-50), "completion_tokens": float64(-7),
	}, "")
	if u.In < 0 || u.Out < 0 {
		t.Fatalf("usage in=%d out=%d, want no negatives", u.In, u.Out)
	}
}

func TestOnlyUniqueIsAConflict(t *testing.T) {
	if isUniqueViolation(errors.New("constraint failed: FOREIGN KEY constraint failed")) {
		t.Fatal("a foreign key failure was read as a name conflict")
	}
	if !isUniqueViolation(errors.New("constraint failed: UNIQUE constraint failed: accounts.name")) {
		t.Fatal("a unique failure was missed")
	}
}
