package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

const countBody = `{"model":"%s","system":"be brief","messages":[{"role":"user","content":"count these words for me please"}]}`

// addNativeAnthropic registers an account that speaks the Anthropic wire
// natively against the harness upstream, serving model m1.
func addNativeAnthropic(t *testing.T, h *harness) {
	t.Helper()
	ctx := context.Background()
	acct, err := h.db.UpsertAccount(ctx, provider.Account{Name: "nat", Namespace: "nat",
		Kind: provider.KindAnthropicCompat, BaseURL: h.upstream.URL, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.db.AddKey(ctx, acct, "primary", "sk-ant-native-123456"); err != nil {
		t.Fatal(err)
	}
	if err := h.db.UpsertModels(ctx, acct, []provider.ModelInfo{{ID: "m1", OwnedBy: "nat"}}); err != nil {
		t.Fatal(err)
	}
	yes := true
	if err := h.db.SetModelProto(ctx, acct, "m1", provider.SurfaceAnthropic, &yes); err != nil {
		t.Fatal(err)
	}
}

func countOf(t *testing.T, body string) int64 {
	t.Helper()
	var out struct {
		InputTokens int64 `json:"input_tokens"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("body %s: %v", body, err)
	}
	return out.InputTokens
}

func TestCountTokensAsksTheNativeUpstream(t *testing.T) {
	var model atomic.Value
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages/count_tokens" {
			http.NotFound(w, r)
			return
		}
		var in struct{ Model string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		model.Store(in.Model + "|" + r.Header.Get("x-api-key"))
		w.Write([]byte(`{"input_tokens":42}`))
	})
	addNativeAnthropic(t, h)
	code, hdr, body := h.do("POST", "/v1/messages/count_tokens", "tok-infer", strings.Replace(countBody, "%s", "nat/m1", 1))
	if code != 200 || countOf(t, body) != 42 || hdr.Get(countTokensSourceHeader) != "upstream" {
		t.Fatalf("code=%d source=%q body=%s", code, hdr.Get(countTokensSourceHeader), body)
	}
	if got, _ := model.Load().(string); got != "m1|sk-ant-native-123456" {
		t.Fatalf("upstream saw %q, want the hop's model and key", got)
	}
}

func TestCountTokensEstimatesWhenUpstreamLacksIt(t *testing.T) {
	var hits atomic.Int64
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.NotFound(w, r)
	})
	addNativeAnthropic(t, h)
	before := countTokensServed.estimate.Load()
	code, hdr, body := h.do("POST", "/v1/messages/count_tokens", "tok-infer", strings.Replace(countBody, "%s", "nat/m1", 1))
	if code != 200 || countOf(t, body) <= 1 || hdr.Get(countTokensSourceHeader) != "estimate" {
		t.Fatalf("code=%d source=%q body=%s", code, hdr.Get(countTokensSourceHeader), body)
	}
	if hits.Load() != 1 || countTokensServed.estimate.Load() != before+1 {
		t.Fatalf("upstream hits=%d estimate delta=%d", hits.Load(), countTokensServed.estimate.Load()-before)
	}
}

// A hop that is not on the Anthropic wire (here: OpenAI-only, translated) has
// no count endpoint to ask; the estimate answers without an upstream call.
func TestCountTokensSkipsNonAnthropicHops(t *testing.T) {
	var hits atomic.Int64
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { hits.Add(1) },
		func(o *Options) { o.Resolver = o.Resolver.WithTranslation(true) })
	acct, err := h.db.AccountByNamespace(context.Background(), "up")
	if err != nil {
		t.Fatal(err)
	}
	no := false
	if err := h.db.SetModelProto(context.Background(), acct.ID, "gpt-6-luna", provider.SurfaceAnthropic, &no); err != nil {
		t.Fatal(err)
	}
	yes := true
	if err := h.db.SetModelProto(context.Background(), acct.ID, "gpt-6-luna", provider.SurfaceChat, &yes); err != nil {
		t.Fatal(err)
	}
	code, hdr, body := h.do("POST", "/v1/messages/count_tokens", "tok-infer", strings.Replace(countBody, "%s", "up/gpt-6-luna", 1))
	if code != 200 || hdr.Get(countTokensSourceHeader) != "estimate" || countOf(t, body) <= 1 || hits.Load() != 0 {
		t.Fatalf("code=%d source=%q hits=%d body=%s", code, hdr.Get(countTokensSourceHeader), hits.Load(), body)
	}
}

func TestCountTokensRefusesBadRequests(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	if code, _, _ := h.do("POST", "/v1/messages/count_tokens", "", `{"model":"up/gpt-6-luna"}`); code != 401 {
		t.Fatalf("no token = %d", code)
	}
	if code, _, _ := h.do("POST", "/v1/messages/count_tokens", "tok-infer", `{"messages":[]}`); code != 400 {
		t.Fatalf("no model = %d", code)
	}
	if code, _, _ := h.do("POST", "/v1/messages/count_tokens", "tok-infer", `{"model":"nope/x","messages":[]}`); code == 200 {
		t.Fatal("unknown model counted")
	}
}

// TestCountTokensEstimateLeansHigh: the fallback must not undercount, or a
// client compacts too late. Anthropic measured about 2.35 characters per token
// on prose and code; the old 4-per-token estimate came out ~40% low.
func TestCountTokensEstimateLeansHigh(t *testing.T) {
	text := strings.Repeat("The bridge answers count_tokens with an estimate. ", 200) // 10,000 chars
	got := estimatePromptTokens(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": text}},
	})
	if floor := int64(len(text) * 100 / 235); got < floor {
		t.Fatalf("estimate %d is below the measured Anthropic density floor %d", got, floor)
	}
}
