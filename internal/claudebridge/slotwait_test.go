package claudebridge

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
	req.Header.Set("x-api-key", "tok")
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get(bridgeutil.SlotWaitHeader) == "" {
		t.Fatalf("status %d, slot wait header %q", rec.Code, rec.Header().Get(bridgeutil.SlotWaitHeader))
	}
}

type slottedExecutor struct{ *fakeExec }

func (s slottedExecutor) Run(ctx context.Context, req Request, alias string, sink func(Frame) error) (Result, error) {
	release, err := bridgeutil.NewSlots(1, 1).Acquire(ctx)
	if err != nil {
		return Result{}, err
	}
	defer release()
	return s.fakeExec.Run(ctx, req, alias, sink)
}

func TestStreamingReportsSlotWaitWithFirstEvent(t *testing.T) {
	exec := slottedExecutor{&fakeExec{frames: fixtureFrames(t, "m2_single.jsonl")}}
	rec := post(t, testServer(exec), weatherToolRequest+`,"stream":true}`)
	resp := rec.Result()
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get(bridgeutil.SlotWaitHeader) == "" || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status=%d headers=%v body=%s", resp.StatusCode, resp.Header, rec.Body)
	}
}
