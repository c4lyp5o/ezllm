package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/store"
)

func TestExpiredClientBudgetAnswers429(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	id, err := h.db.AddClientToken(context.Background(), "budget-key", "tok-budget", "infer")
	if err != nil {
		t.Fatal(err)
	}
	limit := int64(100)
	if err := h.db.UpdateToken(context.Background(), id, nil, nil, &limit, "daily", nil, nil); err != nil {
		t.Fatal(err)
	}
	// Spend the whole budget in the ledger.
	h.db.RecordCall(store.Call{
		TS: time.Now().UTC(), Client: "budget-key", AccountID: 1, Model: "m",
		TokensIn: 60, TokensOut: 40, Status: 200,
	})
	if err := h.db.Flush(); err != nil {
		t.Fatal(err)
	}

	code, _, body := h.do("GET", "/v1/models", "tok-budget", "")
	if code != 429 {
		t.Fatalf("spent key = %d, want 429: %s", code, body)
	}
}
