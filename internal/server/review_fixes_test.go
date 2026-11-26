package server

import (
	"fmt"
	"github.com/c4lyp5o/ezllm/internal/claudeweb"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/store"
)

// Renaming a combo in the editor sends PATCH with the new name. It used to
// upsert by name: a second combo appeared and the old one stayed.
func TestComboPatchRenamesInPlace(t *testing.T) {
	h := newHarness(t, (&fakeUpstream{models: []string{"m"}}).handler())
	acct := seededAccount(t, h)
	code, resp := jsonDo(t, h, "POST", "/admin/combos", "tok-admin", map[string]any{
		"name": "old", "strategy": "failover",
		"hops": []any{map[string]any{"account_id": acct, "model_id": "m", "weight": 1}},
	})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, resp)
	}
	id := int64(mustJSON(t, resp)["id"].(float64))
	code, resp = jsonDo(t, h, "PATCH", fmt.Sprintf("/admin/combos/%d", id), "tok-admin", map[string]any{"name": "new"})
	if code != http.StatusOK {
		t.Fatalf("rename: %d %s", code, resp)
	}
	combos, err := h.db.ListCombos(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]int64{}
	for _, c := range combos {
		names[c.Name] = c.ID
	}
	if _, ok := names["old"]; ok || names["new"] != id {
		t.Fatalf("combos after rename = %v, want only new with id %d", names, id)
	}
	if len(combos[0].Hops) == 0 {
		for _, c := range combos {
			if c.ID == id && len(c.Hops) == 0 {
				t.Fatal("the rename dropped the hops")
			}
		}
	}
	code, _ = jsonDo(t, h, "PATCH", fmt.Sprintf("/admin/combos/%d?enabled=yes", id), "tok-admin", map[string]any{})
	if code != http.StatusBadRequest {
		t.Fatalf("?enabled=yes = %d, want 400", code)
	}
}

// count_tokens reads and parses a whole body like inference does, so it must
// queue behind the same request limits instead of bypassing them.
func TestCountTokensIsAdmitted(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {}, func(o *Options) {
		o.RequestLimits = RequestLimits{GlobalInflight: 1, PerClientInflight: 1, BodyMemoryMiB: 64}
	})
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"x"}`))
	r.Header.Set("Authorization", "Bearer tok-infer")
	held := h.server.admit(httptest.NewRecorder(), r)
	if held == nil {
		t.Fatal("first lease refused")
	}
	defer held.release()
	code, _, body := h.do("POST", "/v1/messages/count_tokens", "tok-infer", `{"model":"up/gpt-6-luna","messages":[{"role":"user","content":"hi"}]}`)
	if code != http.StatusTooManyRequests {
		t.Fatalf("count_tokens with the limit full = %d %s, want 429", code, body)
	}
}

// A partial rule PATCH keeps what it leaves out: {"enabled":false} used to
// zero cap_tokens and delete the allowed-hours window.
func TestModelRulePatchKeepsOmittedFields(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	_, m := postRule(t, h, `{"account_id":1,"model_id":"gpt-6-luna","cap_tokens":5000,"cap_window":"daily","win_start":"09:00","win_end":"17:00","win_days":"1,2,3","win_tz":"UTC","enabled":true,"note":"keep"}`)
	id := int64(m["id"].(float64))
	code, _, resp := h.do("PATCH", fmt.Sprintf("/admin/model-rules/%d", id), adminTok(), `{"enabled":false}`)
	if code != http.StatusOK {
		t.Fatalf("patch = %d %s", code, resp)
	}
	got, err := h.db.GetModelRule(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled || got.CapTokens != 5000 || got.WinStart != "09:00" || got.WinEnd != "17:00" || got.WinDays != "1,2,3" || got.Note != "keep" {
		t.Fatalf("after {enabled:false}: %+v", got)
	}
	code, _, resp = h.do("PATCH", fmt.Sprintf("/admin/model-rules/%d", id), adminTok(), `{"win_start":"","win_end":"","win_days":"","note":""}`)
	if code != http.StatusOK {
		t.Fatalf("clear = %d %s", code, resp)
	}
	got, _ = h.db.GetModelRule(t.Context(), id)
	if got.WinStart != "" || got.WinEnd != "" || got.Note != "" || got.CapTokens != 5000 {
		t.Fatalf("explicit clear: %+v", got)
	}
}

// An import with one bad row writes nothing; it used to keep the rows before it.
func TestPriceImportIsAllOrNothing(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	for _, bad := range []string{
		`{"account_id":1,"model_id":"no-such-model","effective_from":"2026-01-02","price_in":1,"price_out":2,"price_cache_read":0,"price_cache_write":0}`,
		`{"account_id":1,"model_id":"gpt-6-luna","effective_from":"2026-01-02","price_in":-1,"price_out":2,"price_cache_read":0,"price_cache_write":0}`,
	} {
		payload := `{"source":"test","prices":[
			{"account_id":1,"model_id":"gpt-6-luna","effective_from":"2026-01-01","price_in":1,"price_out":2,"price_cache_read":0.1,"price_cache_write":0.2},` + bad + `]}`
		code, _, body := h.do("POST", "/admin/prices/import", adminTok(), payload)
		if code != http.StatusBadRequest {
			t.Fatalf("import with a bad row = %d %s, want 400", code, body)
		}
		prices, err := h.db.ListPrices(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(prices) != 0 {
			t.Fatalf("a refused import still wrote %d row(s)", len(prices))
		}
	}
}

// A sync body sent without Content-Length (chunked) is read, not skipped, and
// a surface name ezllm does not know is refused instead of probing all three.
func TestSyncReadsChunkedBodyAndRefusesUnknownSurface(t *testing.T) {
	h := newHarness(t, (&fakeUpstream{models: []string{"m"}}).handler())
	for _, chunked := range []bool{false, true} {
		r := httptest.NewRequest("POST", "/admin/accounts/1/sync", strings.NewReader(`{"surfaces":["typo"]}`))
		if chunked {
			r.ContentLength = -1
		}
		r.Header.Set("Authorization", "Bearer tok-admin")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.handler.ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "surfaces") {
			t.Fatalf("chunked=%v: %d %s, want 400 naming surfaces", chunked, w.Code, w.Body.String())
		}
	}
}

// The dashboard's on/off switch sends {"enabled": false} alone. It used to wipe
// the account's custom headers, token cap, context size, quota mode and cap
// window. A PATCH also gets the create path's validation.
func TestAccountPatchKeepsOmittedFields(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	capTok, maxCtx := int64(7000), int64(128000)
	id, err := h.db.CreateAccount(t.Context(), store.AccountInput{
		Name: "acme", Namespace: "acme", Kind: "openai-compatible", BaseURL: "https://api.example.com/v1",
		CustomHeaders: map[string]string{"X-Team": "a"}, CapTokens: &capTok, MaxContextTokens: &maxCtx,
		QuotaMode: "tokens", CapWindow: "daily",
	})
	if err != nil {
		t.Fatal(err)
	}
	code, _, resp := h.do("PATCH", fmt.Sprintf("/admin/accounts/%d", id), adminTok(), `{"enabled":false}`)
	if code != http.StatusOK {
		t.Fatalf("patch = %d %s", code, resp)
	}
	a, err := h.db.AccountByID(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if a.Enabled || a.CustomHeaders["X-Team"] != "a" || a.CapTokens != 7000 || a.MaxContextTokens != 128000 || a.QuotaMode != "tokens" || a.CapWindow != "daily" {
		t.Fatalf("after {enabled:false}: enabled=%v headers=%v cap=%d ctx=%d quota=%q window=%q",
			a.Enabled, a.CustomHeaders, a.CapTokens, a.MaxContextTokens, a.QuotaMode, a.CapWindow)
	}
	code, _, _ = h.do("PATCH", fmt.Sprintf("/admin/accounts/%d", id), adminTok(), `{"namespace":"a/b"}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("namespace with '/' = %d, want 422 like create", code)
	}
}

// Changing a rule's model moves that rule; it used to leave the old rule in
// force and add a second one. Taking another rule's pair answers 409.
func TestModelRulePatchMovesInPlace(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	_, a := postRule(t, h, `{"account_id":1,"model_id":"model-a","cap_tokens":10,"cap_window":"daily","enabled":true}`)
	postRule(t, h, `{"account_id":1,"model_id":"model-b","cap_tokens":10,"cap_window":"daily","enabled":true}`)
	id := int64(a["id"].(float64))
	code, _, resp := h.do("PATCH", fmt.Sprintf("/admin/model-rules/%d", id), adminTok(), `{"model_id":"model-c"}`)
	if code != http.StatusOK {
		t.Fatalf("move = %d %s", code, resp)
	}
	rules, err := h.db.ListModelRules(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	models := map[string]int64{}
	for _, r := range rules {
		models[r.ModelID] = r.ID
	}
	if len(rules) != 2 || models["model-c"] != id {
		t.Fatalf("rules after move = %v, want model-b and model-c (id %d)", models, id)
	}
	code, _, _ = h.do("PATCH", fmt.Sprintf("/admin/model-rules/%d", id), adminTok(), `{"model_id":"model-b"}`)
	if code != http.StatusConflict {
		t.Fatalf("taking model-b's pair = %d, want 409", code)
	}
}

type webMsg = struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// developer messages are the newer system messages: neither reaches a web
// provider. An image is refused, not silently dropped.
func TestWebBridgeDropsDeveloperAndSeesImages(t *testing.T) {
	msgs := []webMsg{{Role: "developer", Content: "secret persona"}, {Role: "user", Content: "hello"}}
	if got := flattenWebBridgeMessages(msgs); strings.Contains(got, "secret persona") || got != "hello" {
		t.Fatalf("prompt = %q, want only the user text", got)
	}
	img := []webMsg{{Role: "user", Content: []any{
		map[string]any{"type": "text", "text": "what is this"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}},
	}}}
	if !webBridgeHasImage(img) || webBridgeHasImage(msgs) {
		t.Fatal("image detection is wrong")
	}
}

// A claude-web turn that answers with a tool call is a 400 naming
// chat_tools_unsupported, so a combo moves on to a hop that serves tools.
func TestClaudeWebToolCallIsA400(t *testing.T) {
	if got := claudeWebStatus(claudeweb.ErrToolCall); got != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", got)
	}
}
