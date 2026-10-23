package server

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/store"
)

func TestAccountingUnavailableMeterBlocksCappedClient(t *testing.T) {
	var calls atomic.Int64
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	ctx := context.Background()
	id, err := h.db.AddClientToken(ctx, "meter-client", "meter-token", "infer")
	if err != nil {
		t.Fatal(err)
	}
	cap := int64(100)
	if err = h.db.UpdateToken(ctx, id, nil, nil, &cap, "daily", nil, nil); err != nil {
		t.Fatal(err)
	}
	h.db.RecordCall(store.Call{TS: time.Now().UTC(), Client: "meter-client", TokensIn: 1})
	if err = h.db.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err = h.db.Writer().Exec(`CREATE TRIGGER meter_failure BEFORE INSERT ON client_usage_counters WHEN NEW.tokens >= 99 BEGIN SELECT RAISE(ABORT,'meter fault'); END`); err != nil {
		t.Fatal(err)
	}
	h.db.RecordCall(store.Call{TS: time.Now().UTC(), Client: "meter-client", TokensOut: 99})
	if err = h.db.Flush(); err != nil {
		t.Fatal(err)
	}
	code, _, body := h.do("POST", "/v1/chat/completions", "meter-token", `{"model":"up/gpt-6-luna","messages":[{"role":"user","content":"hi"}],"max_tokens":1}`)
	if code != 503 || calls.Load() != 0 {
		t.Fatalf("uncounted spend admitted: %d %s upstream=%d", code, body, calls.Load())
	}
	if code, _, body = h.do("GET", "/admin/overview", adminTok(), ""); code != 200 {
		t.Fatalf("uncapped operator locked out: %d %s", code, body)
	}
	if _, err = h.db.Writer().Exec(`DROP TRIGGER meter_failure`); err != nil {
		t.Fatal(err)
	}
	if code, _, body = h.do("GET", "/v1/models", "meter-token", ""); code != 429 {
		t.Fatalf("reconciled cap not enforced: %d %s", code, body)
	}
}
