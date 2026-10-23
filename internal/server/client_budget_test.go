package server

import (
	"context"
	"net/http"
	"net/http/httptest"
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

// TestBudgetReservationHeldUntilUsageIsCounted pins audit finding 5: the
// handler released its reservation on return while the request's usage still
// sat in the ledger queue, so a second request could reserve the budget the
// first had just spent. The reservation must hold until the row that counts
// the usage commits.
func TestBudgetReservationHeldUntilUsageIsCounted(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"gpt-6-luna",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":60,"completion_tokens":40,"total_tokens":100}}`))
	}))
	// Rows stay queued until Flush: the gap between response and ledger
	// commit, held open deterministically.
	h := newHarnessWithStore(t, up, func(o *store.Options) { o.BatchSize = 1000; o.BatchWait = time.Hour })
	ctx := context.Background()
	id, err := h.db.AddClientToken(ctx, "budget-key", "tok-budget", "infer")
	if err != nil {
		t.Fatal(err)
	}
	limit := int64(100)
	if err := h.db.UpdateToken(ctx, id, nil, nil, &limit, "daily", nil, nil); err != nil {
		t.Fatal(err)
	}
	req := `{"model":"up/gpt-6-luna","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`

	if code, _, body := h.do("POST", "/v1/chat/completions", "tok-budget", req); code != 200 {
		t.Fatalf("first request = %d, want 200: %s", code, body)
	}
	// The first request spent all 100 tokens; its row is still queued.
	if code, _, body := h.do("POST", "/v1/chat/completions", "tok-budget", req); code != 429 {
		t.Fatalf("second request before the ledger flush = %d, want 429 (budget already spent): %s", code, body)
	}
	if err := h.db.Flush(); err != nil {
		t.Fatal(err)
	}
	if code, _, body := h.do("POST", "/v1/chat/completions", "tok-budget", req); code != 429 {
		t.Fatalf("request after the flush = %d, want 429: %s", code, body)
	}
}
