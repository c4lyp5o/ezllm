package server

// M5 at the server boundary: the universal cell is actually SELECTED and
// served end to end (config + resolver + proxy wired), and a stream that ends
// in a late error must not move an outcome-driven cursor — the hop did not
// serve the request even though the delivered status was 200.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/router"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// matrixUpstream answers on the path its wire implies and can cut a stream
// after one delta; it records the Authorization key so the test can tell the
// hops apart (each account carries its own credential).
type matrixUpstream struct {
	mu     sync.Mutex
	calls  []string // "<key> <path>"
	cutNth int      // when >0: the Nth stream request dies after one delta
	n      int
}

func (m *matrixUpstream) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.n++
		n := m.n
		m.calls = append(m.calls, r.Header.Get("Authorization")+" "+r.URL.Path)
		cut := m.cutNth == n
		m.mu.Unlock()

		body, _ := io.ReadAll(r.Body)
		var req struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &req)
		chat := strings.HasSuffix(r.URL.Path, "/chat/completions")
		if !req.Stream {
			w.Header().Set("Content-Type", "application/json")
			if chat {
				_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
			} else {
				// The Messages wire: a chat client arrives here translated.
				_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":5}}`))
			}
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if chat {
			_, _ = w.Write([]byte(`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}` + "\n\n"))
		} else {
			_, _ = w.Write([]byte(`event: message_start` + "\n" +
				`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"m","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":4,"output_tokens":1}}}` + "\n\n" +
				`event: content_block_start` + "\n" +
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
				`event: content_block_delta` + "\n" +
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}` + "\n\n"))
		}
		if cut {
			return // the stream dies here: no terminal event at all
		}
		if chat {
			_, _ = w.Write([]byte(`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}` + "\n\n" + "data: [DONE]\n\n"))
			return
		}
		_, _ = w.Write([]byte(`event: content_block_stop` + "\n" +
			`data: {"type":"content_block_stop","index":0}` + "\n\n" +
			`event: message_delta` + "\n" +
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5}}` + "\n\n" +
			`event: message_stop` + "\n" +
			`data: {"type":"message_stop"}` + "\n\n"))
	}
}

// markMessagesOnly proves a model serves the Messages wire and NOT chat, and
// gives the account the explicit usage convention a Messages destination
// requires.
func markMessagesOnly(t *testing.T, db *store.DB, acctID int64) {
	t.Helper()
	no := false
	yes := true
	for _, s := range []provider.Surface{provider.SurfaceOpenAI, provider.SurfaceResponses} {
		if err := db.SetModelProto(context.Background(), acctID, "gpt-6-luna", s, &no); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.SetModelProto(context.Background(), acctID, "gpt-6-luna", provider.SurfaceAnthropic, &yes); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateAccount(context.Background(), acctID, store.AccountInput{
		UsageConvention: string(provider.UsageConventionTotal),
	}); err != nil {
		t.Fatal(err)
	}
}

// A chat client on a Messages-only hop: with universal ON the router must
// select the NEW chat→messages cell, the upstream must see /v1/messages with
// a messages-shaped body, and the client a chat-shaped response.
func TestServerUniversalCellEndToEnd(t *testing.T) {
	up := &matrixUpstream{}
	srv := httptest.NewServer(up.handler())
	defer srv.Close()
	h := newHarness(t, up.handler(), func(o *Options) {
		o.Resolver = router.New(o.DB).WithTranslation(true).WithUniversal(true)
	})
	ctx := context.Background()
	acct, err := h.db.AccountByNamespace(ctx, "up")
	if err != nil {
		t.Fatal(err)
	}
	markMessagesOnly(t, h.db, acct.ID)

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"up/gpt-6-luna","messages":[{"role":"user","content":"hi"}],"max_tokens":64}`))
	req.Header.Set("Authorization", "Bearer tok-infer")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	up.mu.Lock()
	defer up.mu.Unlock()
	if len(up.calls) != 1 || !strings.HasSuffix(up.calls[0], "/messages") {
		t.Fatalf("upstream calls = %v, want exactly one on the messages path", up.calls)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("client body %s: %v", rec.Body.String(), err)
	}
	if resp["object"] == "chat.completion" || resp["type"] == "message" {
		if resp["type"] == "message" {
			t.Fatalf("a chat client received a messages body: %s", rec.Body.String())
		}
	}
	if !strings.Contains(rec.Body.String(), `"content":"hello"`) && !strings.Contains(rec.Body.String(), `"content": "hello"`) {
		t.Fatalf("client response lost the text: %s", rec.Body.String())
	}
}

// A late stream error must be recorded honestly. The client already received
// a 200 and bytes, so the status cannot change — the row keeps the delivered
// status and carries the failure, and the positive hop signal is withheld (the
// server gates it on Result.StreamCompleted()).
//
// Translated hops are ordinary combo members (operator decision 2026-10-13):
// the withheld signal feeds the strategies exactly like a native failure.
func TestServerLateStreamErrorRecordedNotMarkedGood(t *testing.T) {
	up := &matrixUpstream{}
	h := newHarness(t, up.handler(), func(o *Options) {
		o.Resolver = router.New(o.DB).WithTranslation(true).WithUniversal(true)
	})
	ctx := context.Background()
	acct, err := h.db.AccountByNamespace(ctx, "up")
	if err != nil {
		t.Fatal(err)
	}
	markMessagesOnly(t, h.db, acct.ID)

	// A stream that dies after its first delta (no terminal event at all).
	up.mu.Lock()
	up.cutNth = 1
	up.mu.Unlock()

	body := `{"model":"up/gpt-6-luna","messages":[{"role":"user","content":"hi"}],"max_tokens":64,"stream":true}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok-infer")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want the committed 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "partial") {
		t.Fatalf("client lost the delivered fragment: %s", rec.Body.String())
	}
	up.mu.Lock()
	calls := len(up.calls)
	up.mu.Unlock()
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1 (no retry after commit)", calls)
	}

	// The ledger is batched (BatchWait 20ms): poll briefly rather than sleep.
	var row store.Call
	for i := 0; i < 100; i++ {
		rows, rerr := h.db.RecentCalls(ctx, 1)
		if rerr == nil && len(rows) > 0 {
			row = rows[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if row.Status == 0 {
		t.Fatal("no ledger row appeared within the batch window")
	}
	if row.Status != 200 {
		t.Errorf("ledger status = %d, want the delivered 200", row.Status)
	}
	if !row.Translated {
		t.Errorf("ledger lost the translated attribution")
	}
	if !strings.Contains(row.Err, "stream") {
		t.Errorf("ledger err = %q, want the delivery outcome recorded", row.Err)
	}
}
