package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestClientAllowsModelPatterns(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "p.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	id, err := db.AddClientToken(ctx, "scoped-key", "tok-scoped", "infer")
	if err != nil {
		t.Fatal(err)
	}
	allowed := "deepseek/*, openai/gpt-5*"
	if err := db.UpdateToken(ctx, id, nil, nil, nil, "", &allowed, nil); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		model string
		want  bool
	}{
		{"deepseek/deepseek-chat", true},
		{"openai/gpt-5-nano", true},
		{"openai/gpt-4o", false},
		{"anthropic/claude", false},
	}
	for _, c := range cases {
		got, err := db.ClientAllowsModel(ctx, "scoped-key", c.model)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("allows %q = %v, want %v", c.model, got, c.want)
		}
	}
	// Empty list = every model.
	if err := db.UpdateToken(ctx, id, nil, nil, nil, "", strPtr(""), nil); err != nil {
		t.Fatal(err)
	}
	if ok, _ := db.ClientAllowsModel(ctx, "scoped-key", "anything/at-all"); !ok {
		t.Fatal("empty allowlist must allow every model")
	}
}

func TestClientTokenExpiry(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "e.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	if _, _, err := db.UpsertToken(ctx, TokenInput{
		Name: "expired-key", Roles: []string{"infer"}, Token: "tok-expired", ExpiresAt: past,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ClientByToken(ctx, "tok-expired"); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("expired token err = %v, want ErrTokenExpired", err)
	}

	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if _, _, err := db.UpsertToken(ctx, TokenInput{
		Name: "live-key", Roles: []string{"infer"}, Token: "tok-live", ExpiresAt: future,
	}); err != nil {
		t.Fatal(err)
	}
	if name, _, err := db.ClientByToken(ctx, "tok-live"); err != nil || name != "live-key" {
		t.Fatalf("live token = %q, %v", name, err)
	}
}

func TestReserveClientBudget(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "r.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	id, err := db.AddClientToken(ctx, "reserve-key", "tok-reserve", "infer")
	if err != nil {
		t.Fatal(err)
	}
	limit := int64(100)
	if err := db.UpdateToken(ctx, id, nil, nil, &limit, "daily", nil, nil); err != nil {
		t.Fatal(err)
	}
	resID, ok, err := db.ReserveClientBudget(ctx, "reserve-key", 60)
	if err != nil || !ok || resID == 0 {
		t.Fatalf("first reserve = id %d ok %v err %v", resID, ok, err)
	}
	if _, ok, err := db.ReserveClientBudget(ctx, "reserve-key", 50); err != nil || ok {
		t.Fatalf("second reserve should be denied, ok=%v err=%v", ok, err)
	}
	db.ReleaseClientBudget(ctx, resID)
	if _, ok, err := db.ReserveClientBudget(ctx, "reserve-key", 50); err != nil || !ok {
		t.Fatalf("after release reserve should pass, ok=%v err=%v", ok, err)
	}
}

func strPtr(s string) *string { return &s }

// The ledger row that counts a request's usage ends its reservation in the
// same commit (audit finding 5), so a reservation never outlives its row
// and the budget is never both unreserved and uncounted.
func TestFlushReleasesTheRowsReservation(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "r.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	id, err := db.AddClientToken(ctx, "held-key", "tok-held", "infer")
	if err != nil {
		t.Fatal(err)
	}
	limit := int64(100)
	if err := db.UpdateToken(ctx, id, nil, nil, &limit, "daily", nil, nil); err != nil {
		t.Fatal(err)
	}
	resID, ok, err := db.ReserveClientBudget(ctx, "held-key", 100)
	if err != nil || !ok {
		t.Fatalf("reserve = ok %v err %v", ok, err)
	}
	db.RecordCall(Call{TS: time.Now().UTC(), Client: "held-key", Model: "m", Status: 200,
		TokensIn: 60, TokensOut: 40, ReservationID: resID})
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.r.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM client_budget_reservations WHERE id = ?`, resID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("reservation survived the flush that counted its row")
	}
	st, err := db.ClientBudgetStatus(ctx, "held-key")
	if err != nil {
		t.Fatal(err)
	}
	if st.Used != 100 {
		t.Fatalf("used = %d, want the row's 100 tokens", st.Used)
	}
}
