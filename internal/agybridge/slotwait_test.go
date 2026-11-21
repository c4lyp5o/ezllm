package agybridge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/bridgeutil"
)

// Every authenticated route tracks the slot wait, so the proxy can record it
// in the ledger (calls.slot_wait_ms).
func TestAuthedReportsTheSlotWait(t *testing.T) {
	slots := bridgeutil.NewSlots(1, 1)
	h := (&Server{Token: "tok"}).authed(func(w http.ResponseWriter, r *http.Request) {
		release, err := slots.Acquire(r.Context())
		if err != nil {
			t.Fatal(err)
		}
		release()
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(context.Background())
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get(bridgeutil.SlotWaitHeader) == "" {
		t.Fatalf("status %d, slot wait header %q", rec.Code, rec.Header().Get(bridgeutil.SlotWaitHeader))
	}
}
