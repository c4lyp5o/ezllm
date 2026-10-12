package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/router"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// Exercise the real resolver, store and dispatcher: a matching OpenAI pin
// must permit an Anthropic client only when the hop actually translates.
func TestPinnedTranslatedComboHTTP(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "off", true: "on"}[enabled], func(t *testing.T) {
			var hits int
			var wirePath string
			var wireBody struct {
				Model    string `json:"model"`
				Messages []struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"messages"`
			}
			h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
				hits++
				wirePath = r.URL.Path
				if err := json.NewDecoder(r.Body).Decode(&wireBody); err != nil {
					t.Errorf("upstream decode: %v", err)
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"id":"chat-proof","choices":[{"message":{"role":"assistant","content":"pin-wire-proof"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2}}`))
			}, func(o *Options) { o.Resolver = router.New(o.DB).WithTranslation(enabled) })
			ctx := context.Background()
			acct := seededAccountID(t, h)
			if err := h.db.SetModelPin(ctx, acct, "gpt-6-luna", "openai"); err != nil {
				t.Fatal(err)
			}
			// Leave capability unknown when OFF to pin the existing pre-dial
			// refusal; ON needs proof of the OpenAI-only path.
			if enabled {
				yes, no := true, false
				for surface, verdict := range map[provider.Surface]*bool{provider.SurfaceOpenAI: &yes, provider.SurfaceAnthropic: &no} {
					if err := h.db.SetModelProto(ctx, acct, "gpt-6-luna", surface, verdict); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := h.db.UpsertCombo(ctx, store.Combo{Name: "pin-proof", Strategy: "failover", Enabled: true, Hops: []store.ComboHop{{AccountID: acct, ModelID: "gpt-6-luna", Weight: 1}}}); err != nil {
				t.Fatal(err)
			}
			code, _, body := h.do("POST", "/v1/messages", "tok-infer", `{"model":"pin-proof","max_tokens":8,"messages":[{"role":"user","content":[{"type":"text","text":"ping"}]}]}`)
			if !enabled {
				const want = "{\"error\":{\"code\":400,\"message\":\"model \\\"pin-proof\\\" is pinned to the openai surface; this request used anthropic — call it on the openai endpoint instead\",\"pinned\":\"openai\",\"requested_surface\":\"anthropic\",\"type\":\"pinned_surface\"}}\n"
				if code != 400 || hits != 0 || body != want {
					t.Fatalf("OFF refusal changed: code=%d hits=%d body=%s", code, hits, body)
				}
				return
			}
			if code != 200 {
				t.Fatalf("translated pinned combo = %d: %s", code, body)
			}
			if hits != 1 || wirePath != "/chat/completions" || wireBody.Model != "gpt-6-luna" || len(wireBody.Messages) != 1 || wireBody.Messages[0].Role != "user" || wireBody.Messages[0].Content != "ping" {
				t.Fatalf("wrong upstream wire: path=%q hits=%d body=%+v", wirePath, hits, wireBody)
			}
			var response struct {
				Type    string                        `json:"type"`
				Model   string                        `json:"model"`
				Content []struct{ Type, Text string } `json:"content"`
			}
			if err := json.Unmarshal([]byte(body), &response); err != nil {
				t.Fatal(err)
			}
			if response.Type != "message" || response.Model != "pin-proof" || len(response.Content) != 1 || response.Content[0].Type != "text" || response.Content[0].Text != "pin-wire-proof" {
				t.Fatalf("client must receive translated Anthropic response: %s", body)
			}
		})
	}
}
