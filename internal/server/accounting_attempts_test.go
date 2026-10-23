package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/store"
)

func TestAccountingRetriesAreLedgered(t *testing.T) {
	for _, finalFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "final_failure"}[finalFailure], func(t *testing.T) {
			var hits atomic.Int64
			h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
				n := hits.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if n == 1 || finalFailure {
					w.WriteHeader(503)
					io.WriteString(w, `{"error":{"message":"failed after work"},"usage":{"prompt_tokens":100,"completion_tokens":42}}`)
					return
				}
				io.WriteString(w, `{"id":"c","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
			})
			h.server.dispatcher.SetRetries(1)
			code, _, body := h.do("POST", "/v1/chat/completions", "tok-infer", `{"model":"up/gpt-6-luna","messages":[{"role":"user","content":"hi"}],"max_tokens":50}`)
			if hits.Load() != 2 {
				t.Fatalf("hits=%d code=%d body=%s", hits.Load(), code, body)
			}
			if err := h.db.Flush(); err != nil {
				t.Fatal(err)
			}
			rows, err := h.db.RecentCalls(context.Background(), 20)
			if err != nil {
				t.Fatal(err)
			}
			var missing int
			if err := h.db.Reader().QueryRow(`SELECT COUNT(*) FROM calls WHERE model='gpt-6-luna' AND (account_id IS NULL OR account_id=0)`).Scan(&missing); err != nil || missing != 0 {
				t.Fatalf("missing account IDs: %d %v", missing, err)
			}
			var in, out int64
			count := 0
			for _, c := range rows {
				if c.Client == "test-infer" || c.Model == "gpt-6-luna" {
					count++
					in += c.TokensIn
					out += c.TokensOut
					if c.Account != "up" || c.KeyID == 0 {
						t.Fatal("lost attribution")
					}
				}
			}
			wantIn, wantOut := int64(110), int64(47)
			if finalFailure {
				wantIn, wantOut = 200, 84
			}
			if count != 2 || in != wantIn || out != wantOut {
				t.Fatalf("attempt spend missing/doubled: rows=%+v count=%d in=%d out=%d", rows, count, in, out)
			}
		})
	}
}

func TestAccountingFailoverAttribution(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream_%t", stream), func(t *testing.T) {
			h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(503)
					io.WriteString(w, "data: {\"error\":{\"message\":\"failed after work\"},\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":42}}\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(503)
				io.WriteString(w, `{"error":{"message":"failed after work"},"usage":{"prompt_tokens":100,"completion_tokens":42}}`)
			})
			good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\ndata: [DONE]\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"id":"c","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
			}))
			defer good.Close()
			ctx := context.Background()
			first := seededAccountID(t, h)
			second, err := h.db.UpsertAccount(ctx, provider.Account{Name: "fallback", Namespace: "fallback", Kind: provider.KindOpenAICompatible, BaseURL: good.URL, Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err = h.db.AddKey(ctx, second, "key", "sk-fallback-key"); err != nil {
				t.Fatal(err)
			}
			if err = h.db.UpsertModels(ctx, second, []provider.ModelInfo{{ID: "gpt-6-luna"}}); err != nil {
				t.Fatal(err)
			}
			if _, err = h.db.UpsertCombo(ctx, store.Combo{Name: "attempt-accounting", Strategy: "failover", Enabled: true, Hops: []store.ComboHop{{AccountID: first, ModelID: "gpt-6-luna", Weight: 1}, {AccountID: second, ModelID: "gpt-6-luna", Weight: 1}}}); err != nil {
				t.Fatal(err)
			}
			token, err := h.db.AddClientToken(ctx, "attempt-client", "attempt-token", "infer")
			if err != nil {
				t.Fatal(err)
			}
			cap := int64(1000)
			if err = h.db.UpdateToken(ctx, token, nil, nil, &cap, "daily", nil, nil); err != nil {
				t.Fatal(err)
			}
			h.server.dispatcher.SetRetries(0)
			code, _, body := h.do("POST", "/v1/chat/completions", "attempt-token", fmt.Sprintf(`{"model":"attempt-accounting","messages":[{"role":"user","content":"hi"}],"max_tokens":50,"stream":%t}`, stream))
			if code != 200 {
				t.Fatalf("dispatch %d %s", code, body)
			}
			if err = h.db.Flush(); err != nil {
				t.Fatal(err)
			}
			rows, err := h.db.Reader().Query(`SELECT account_id,tokens_in,tokens_out,provider_key_id FROM calls WHERE client='attempt-client' ORDER BY id`)
			if err != nil {
				t.Fatal(err)
			}
			var got []int64
			for rows.Next() {
				var aid, in, out, key int64
				if err = rows.Scan(&aid, &in, &out, &key); err != nil {
					t.Fatal(err)
				}
				if key == 0 {
					t.Fatal("key attribution lost")
				}
				got = append(got, aid, in, out)
			}
			if err = rows.Err(); err != nil {
				t.Fatal(err)
			}
			rows.Close()
			if !reflect.DeepEqual(got, []int64{first, 100, 42, second, 10, 5}) {
				t.Fatalf("misattributed/doubled attempts: %v", got)
			}
			b, err := h.db.ClientBudgetStatus(ctx, "attempt-client")
			if err != nil || b.Used != 157 {
				t.Fatalf("client meter=%+v err=%v", b, err)
			}
			var reservations int
			if err = h.db.Reader().QueryRow(`SELECT COUNT(*) FROM client_budget_reservations WHERE client='attempt-client'`).Scan(&reservations); err != nil || reservations != 0 {
				t.Fatalf("reservation reconciliation: %d %v", reservations, err)
			}
		})
	}
}
