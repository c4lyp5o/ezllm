package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/store"
)

func TestExpiredTokenAnswers401(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	if _, _, err := h.db.UpsertToken(context.Background(), store.TokenInput{
		Name: "expired", Roles: []string{"infer"}, Token: "tok-expired", ExpiresAt: past,
	}); err != nil {
		t.Fatal(err)
	}
	code, _, body := h.do("GET", "/v1/models", "tok-expired", "")
	if code != 401 {
		t.Fatalf("expired token = %d, want 401: %s", code, body)
	}
}

func TestClientAllowlistAnswers403(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	id, err := h.db.AddClientToken(context.Background(), "scoped", "tok-scoped", "infer")
	if err != nil {
		t.Fatal(err)
	}
	allowed := "up/*"
	if err := h.db.UpdateToken(context.Background(), id, nil, nil, nil, "", &allowed, nil); err != nil {
		t.Fatal(err)
	}
	code, _, body := h.do("POST", "/v1/chat/completions", "tok-scoped",
		`{"model":"other/model","messages":[{"role":"user","content":"hi"}]}`)
	if code != 403 {
		t.Fatalf("disallowed model = %d, want 403: %s", code, body)
	}
}

func TestClientBudgetReservationAnswers429(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	id, err := h.db.AddClientToken(context.Background(), "reserve", "tok-reserve", "infer")
	if err != nil {
		t.Fatal(err)
	}
	limit := int64(100)
	if err := h.db.UpdateToken(context.Background(), id, nil, nil, &limit, "daily", nil, nil); err != nil {
		t.Fatal(err)
	}
	h.db.RecordCall(store.Call{
		TS: time.Now().UTC(), Client: "reserve", AccountID: 1, Model: "m",
		TokensIn: 50, Status: 200,
	})
	if err := h.db.Flush(); err != nil {
		t.Fatal(err)
	}
	code, _, body := h.do("POST", "/v1/chat/completions", "tok-reserve",
		`{"model":"up/model","max_tokens":60,"messages":[{"role":"user","content":"hi"}]}`)
	if code != 429 {
		t.Fatalf("reservation over cap = %d, want 429: %s", code, body)
	}
}
