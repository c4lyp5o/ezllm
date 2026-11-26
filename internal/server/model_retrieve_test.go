package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// GET /v1/models/{model} returns the entry the list carries, for a provider
// model (with its slash) and for a combo; an unknown id is 404 model_not_found.
func TestRetrieveOneModel(t *testing.T) {
	h := newHarness(t, (&fakeUpstream{models: []string{"m"}}).handler())
	acct := seededAccount(t, h)
	if code, resp := jsonDo(t, h, "POST", "/admin/combos", "tok-admin", map[string]any{
		"name": "daily", "strategy": "failover",
		"hops": []any{map[string]any{"account_id": acct, "model_id": "m", "weight": 1}},
	}); code != http.StatusCreated {
		t.Fatalf("create combo: %d %s", code, resp)
	}
	_, _, listBody := h.do("GET", "/v1/models", "tok-infer", "")
	var list struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(listBody), &list); err != nil || len(list.Data) == 0 {
		t.Fatalf("list: %v %s", err, listBody)
	}
	var providerID string
	for _, m := range list.Data {
		if id, _ := m["id"].(string); strings.Contains(id, "/") {
			providerID = id
			break
		}
	}
	if providerID == "" {
		t.Fatalf("no provider model in %s", listBody)
	}
	for _, id := range []string{providerID, "daily"} {
		code, _, body := h.do("GET", "/v1/models/"+id, "tok-infer", "")
		if code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", id, code, body)
		}
		var one map[string]any
		if err := json.Unmarshal([]byte(body), &one); err != nil || one["id"] != id || one["object"] != "model" {
			t.Fatalf("GET %s = %s", id, body)
		}
	}
	code, _, body := h.do("GET", "/v1/models/nope/missing", "tok-infer", "")
	if code != http.StatusNotFound || !strings.Contains(body, "model_not_found") {
		t.Fatalf("unknown model: %d %s", code, body)
	}
	if code, _, _ := h.do("GET", "/v1/models/daily", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("no key: %d, want 401", code)
	}

	// An Anthropic SDK sends anthropic-version and expects Anthropic's shape.
	req := httptest.NewRequest("GET", "/v1/models/daily", nil)
	req.Header.Set("Authorization", "Bearer tok-infer")
	req.Header.Set("anthropic-version", "2023-06-01")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	var anth map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &anth); err != nil || anth["type"] != "model" || anth["id"] != "daily" {
		t.Fatalf("anthropic shape: %d %s", rec.Code, rec.Body.String())
	}
}

// DELETE /admin/cooldowns needs a namespace with a model, and answers with
// the count it cleared; GET lists the parks.
func TestCooldownsEndpoint(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	if code, _, body := h.do("DELETE", "/admin/cooldowns?model=m", "tok-admin", ""); code != http.StatusBadRequest {
		t.Fatalf("model without namespace: %d %s", code, body)
	}
	code, _, body := h.do("DELETE", "/admin/cooldowns?namespace=up", "tok-admin", "")
	if code != http.StatusOK || !strings.Contains(body, `"cleared":0`) {
		t.Fatalf("clear: %d %s", code, body)
	}
	if code, _, body := h.do("GET", "/admin/cooldowns", "tok-admin", ""); code != http.StatusOK || !strings.Contains(body, `"cooldowns":[]`) {
		t.Fatalf("list: %d %s", code, body)
	}
	if code, _, _ := h.do("DELETE", "/admin/cooldowns", "tok-infer", ""); code != http.StatusForbidden {
		t.Fatalf("infer key: %d, want 403", code)
	}
}

// Route explain joins the router's verdict with what is decided outside it:
// the account's context window and the key's allowlist.
func TestRouteExplain(t *testing.T) {
	h := newHarness(t, (&fakeUpstream{models: []string{"m"}}).handler())
	acct := seededAccount(t, h)
	var modelID string
	if err := h.db.Reader().QueryRow(`SELECT model_id FROM models WHERE account_id=? LIMIT 1`, acct).Scan(&modelID); err != nil {
		t.Fatal(err)
	}
	if code, resp := jsonDo(t, h, "POST", "/admin/combos", "tok-admin", map[string]any{
		"name": "daily", "strategy": "failover",
		"hops": []any{map[string]any{"account_id": acct, "model_id": modelID, "weight": 1}, map[string]any{"account_id": acct, "model_id": "ghost", "weight": 1}},
	}); code != http.StatusCreated {
		t.Fatalf("create combo: %d %s", code, resp)
	}
	if _, err := h.db.Writer().Exec(`UPDATE accounts SET max_context_tokens=1000 WHERE id=?`, acct); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Writer().Exec(`UPDATE client_tokens SET allowed_models='other/*' WHERE name='my-app'`); err != nil {
		t.Fatal(err)
	}
	code, resp := jsonDo(t, h, "POST", "/admin/route-explain", "tok-admin", map[string]any{"model": "daily", "client": "my-app"})
	if code != http.StatusOK {
		t.Fatalf("explain: %d %s", code, resp)
	}
	var out struct {
		Kind string `json:"kind"`
		Hops []struct {
			Model, Status, Reason string
		} `json:"hops"`
		Key struct {
			ModelAllowed *bool `json:"model_allowed"`
		} `json:"key"`
	}
	if err := json.Unmarshal([]byte(resp), &out); err != nil {
		t.Fatal(err)
	}
	if out.Kind != "combo" || len(out.Hops) != 2 || out.Hops[1].Status != "skipped" || !strings.Contains(out.Hops[1].Reason, "model list") {
		t.Fatalf("explain = %s", resp)
	}
	if out.Key.ModelAllowed == nil || *out.Key.ModelAllowed {
		t.Fatalf("the key's allowlist excludes daily; got %s", resp)
	}
	// A 5,000-token request does not fit a 1,000-token account.
	_, resp = jsonDo(t, h, "POST", "/admin/route-explain", "tok-admin", map[string]any{"model": "daily", "est_tokens": 5000})
	if !strings.Contains(resp, "context too small") {
		t.Fatalf("est_tokens over the account's window: %s", resp)
	}
	if code, _ := jsonDo(t, h, "POST", "/admin/route-explain", "tok-admin", map[string]any{"model": "daily", "surface": "grpc"}); code != http.StatusBadRequest {
		t.Fatalf("bad surface: %d", code)
	}
	_, resp = jsonDo(t, h, "POST", "/admin/route-explain", "tok-admin", map[string]any{"model": "nope"})
	if !strings.Contains(resp, `"error":"`) || strings.Contains(resp, `"error":""`) {
		t.Fatalf("unknown model must carry the error a request would get: %s", resp)
	}
}
