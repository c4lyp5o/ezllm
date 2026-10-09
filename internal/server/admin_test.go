package server

// M3 admin API tests. The load-bearing assertion is TestKeyFailureStoresNothing:
// the design promise is that a failed key test writes NOTHING to the database,
// so a wrong credential leaves no row to clean up and no half-configured state.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// fakeUpstream is an OpenAI-compatible provider with switchable verdicts, so
// both the pass and the fail path run without spending money. newHarness wires
// one of these in as the upstream.
type fakeUpstream struct {
	models        []string
	authFail      bool // every key rejected by the catalog (bad key)
	inferenceFail bool // chat completion answers 401
	quotaBody     string
	// validKey, when set, makes the catalog AUTHENTICATE: it answers 200 only
	// for that key and 401 otherwise. Without it the catalog is public, and a
	// public catalog cannot prove anything (the N1 class of bug).
	validKey string
}

// bearer extracts the key the adapter sent.
func bearer(r *http.Request) string {
	v := r.Header.Get("Authorization")
	return strings.TrimPrefix(v, "Bearer ")
}

func (f *fakeUpstream) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/models"):
			if f.authFail || (f.validKey != "" && bearer(r) != f.validKey) {
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"error":{"message":"Invalid API key","type":"invalid_request_error","code":"invalid_api_key"}}`))
				return
			}
			var sb strings.Builder
			sb.WriteString(`{"object":"list","data":[`)
			for i, m := range f.models {
				if i > 0 {
					sb.WriteString(",")
				}
				fmt.Fprintf(&sb, `{"id":%q,"object":"model","created":1700000000,"owned_by":"up"}`, m)
			}
			sb.WriteString(`]}`)
			w.Write([]byte(sb.String()))

		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			if f.validKey != "" && bearer(r) != f.validKey {
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"error":{"message":"Invalid API key","type":"invalid_request_error","code":"invalid_api_key"}}`))
				return
			}
			if f.inferenceFail {
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"error":{"message":"This key is not authorized for inference","type":"invalid_request_error"}}`))
				return
			}
			w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"up/gpt-6-luna","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10,"prompt_tokens_details":{"cached_tokens":2}}}`))

		case strings.HasSuffix(r.URL.Path, "/usage"):
			if f.quotaBody == "" {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"error":"nope"}`))
				return
			}
			w.Write([]byte(f.quotaBody))

		default:
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":{"message":"unknown path"}}`))
		}
	}
}

// seededSecret is the credential newHarness stores for the "up" account.
const seededSecret = "«redacted:sk-…»"

// ── helpers ─────────────────────────────────────────────────────────────────

// jsonDo runs a request with a marshalled body through the harness.
func jsonDo(t *testing.T, h *harness, method, path, token string, body any) (int, string) {
	t.Helper()
	raw := ""
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		raw = string(b)
	}
	code, _, resp := h.do(method, path, token, raw)
	return code, resp
}

func mustJSON(t *testing.T, resp string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(resp), &out); err != nil {
		t.Fatalf("decode %q: %v", resp, err)
	}
	return out
}

func errObj(t *testing.T, resp string) map[string]any {
	t.Helper()
	e, _ := mustJSON(t, resp)["error"].(map[string]any)
	if e == nil {
		t.Fatalf("no error object in %s", resp)
	}
	return e
}

func countTable(t *testing.T, h *harness, table string) int {
	t.Helper()
	var n int
	if err := h.db.Reader().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// baseline counts every table the key flow can write to.
func baseline(t *testing.T, h *harness) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, tbl := range []string{"accounts", "provider_keys", "models", "combos",
		"combo_hops", "client_tokens", "quota_snapshots", "calls"} {
		out[tbl] = countTable(t, h, tbl)
	}
	return out
}

// seededAccount resolves the harness's seeded account (namespace "up").
func seededAccount(t *testing.T, h *harness) int64 {
	t.Helper()
	var id int64
	if err := h.db.Reader().QueryRow(`SELECT id FROM accounts WHERE namespace='up'`).Scan(&id); err != nil {
		t.Fatalf("seeded account: %v", err)
	}
	return id
}

// addKey goes through the real admin handler — the assertion is about what the
// HTTP contract persists, not what the store does when called directly.
func addKey(t *testing.T, h *harness, accountID int64, body any) (int, string) {
	t.Helper()
	return jsonDo(t, h, "POST", fmt.Sprintf("/admin/accounts/%d/keys", accountID), "tok-admin", body)
}

// ── THE INVARIANT ───────────────────────────────────────────────────────────

func TestKeyFailureStoresNothing(t *testing.T) {
	fake := &fakeUpstream{models: []string{"m-one"}, authFail: true}
	h := newHarness(t, fake.handler())
	acct := seededAccount(t, h)

	before := baseline(t, h)

	code, resp := addKey(t, h, acct, map[string]any{
		"label": "bad-key", "api_key": "sk-wrong-key-1234567890",
	})

	if code != http.StatusUnprocessableEntity {
		t.Fatalf("bad key must 422, got %d: %s", code, resp)
	}
	e := errObj(t, resp)
	if e["type"] != "key_test_failed" {
		t.Errorf("error.type = %v, want key_test_failed", e["type"])
	}
	if e["step"] != "catalog" {
		t.Errorf("error.step = %v, want catalog (the step that 401'd)", e["step"])
	}
	// The provider's own words survive — that is why upstream text is preserved.
	if msg, _ := e["message"].(string); !strings.Contains(msg, "Invalid API key") {
		t.Errorf("error.message lost the upstream text: %q", msg)
	}

	after := baseline(t, h)
	for tbl, n := range after {
		if n != before[tbl] {
			t.Errorf("%s changed on a FAILED key test: %d → %d (must stay identical)",
				tbl, before[tbl], n)
		}
	}
	if strings.Contains(resp, "sk-wrong-key") {
		t.Errorf("response echoed the submitted credential")
	}
}

func TestSuccessfulKeyPersistsAndHintsOnly(t *testing.T) {
	// catalog authenticates: only the key we submit gets 200
	fake := &fakeUpstream{models: []string{"m-one", "m-two"},
		validKey: "sk-super-secret-value-abcdef123456"}
	h := newHarness(t, fake.handler())
	acct := seededAccount(t, h)

	// labelled differently: the harness already seeds a key named "primary"
	const secret = "sk-super-secret-value-abcdef123456"
	code, resp := addKey(t, h, acct, map[string]any{
		"label": "second", "api_key": secret, "skip_inference": true,
	})

	if code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", code, resp)
	}
	out := mustJSON(t, resp)
	key, _ := out["key"].(map[string]any)
	if key == nil {
		t.Fatalf("201 body has no key object: %v", out)
	}
	if hint, _ := key["hint"].(string); hint == "" {
		t.Errorf("key.hint missing — the UI needs it to identify the credential")
	}
	if strings.Contains(resp, secret) {
		t.Errorf("201 response contains the credential plaintext")
	}
	// The harness seeded one key; this adds exactly one.
	if n := countTable(t, h, "provider_keys"); n != 2 {
		t.Fatalf("provider_keys = %d, want 2", n)
	}
	// The catalog REPLACES the account's model list (UpsertModels deletes then
	// inserts), so the seeded row is gone and these two are authoritative.
	var got []string
	rows, err := h.db.Reader().Query(`SELECT model_id FROM models ORDER BY model_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			t.Fatal(err)
		}
		got = append(got, m)
	}
	if strings.Join(got, ",") != "m-one,m-two" {
		t.Errorf("models = %v, want exactly the catalog [m-one m-two]", got)
	}
	// Stored as plaintext only because this test build explicitly requests it; API responses still expose hints only.
	var stored string
	if err := h.db.Reader().QueryRow(
		`SELECT key_plain FROM provider_keys WHERE label='second'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != secret {
		t.Errorf("stored provider key does not match the tested credential")
	}
	// The list endpoint never re-serves it either.
	_, listResp := jsonDo(t, h, "GET",
		fmt.Sprintf("/admin/accounts/%d/keys", acct), "tok-admin", nil)
	if strings.Contains(listResp, secret) {
		t.Errorf("GET keys leaked the credential")
	}
}

func TestQuotaErrorStillSavesTheKey(t *testing.T) {
	// A quota endpoint that does not exist is NOT a bad key.
	fake := &fakeUpstream{models: []string{"m-one"}, quotaBody: "",
		validKey: "sk-fine-key-000"}
	h := newHarness(t, fake.handler())
	acct := seededAccount(t, h)

	code, resp := addKey(t, h, acct, map[string]any{
		"label": "k", "api_key": "sk-fine-key-000", "skip_inference": true,
	})
	if code != http.StatusCreated {
		t.Fatalf("missing quota endpoint must not block a good key: %d %s", code, resp)
	}
	steps, _ := mustJSON(t, resp)["test"].(map[string]any)["steps"].([]any)
	var quota map[string]any
	for _, s := range steps {
		m, ok := s.(map[string]any)
		if ok && m["step"] == "quota" {
			quota = m
		}
	}
	if quota == nil {
		t.Fatalf("no quota step in evidence: %v", steps)
	}
	if quota["ok"] != false {
		t.Errorf("quota step should be ok=false when the endpoint 404s, got %v", quota["ok"])
	}
	if quota["fatal"] == true {
		t.Errorf("quota failure must never be fatal")
	}
}

func TestInferenceFailureIsFatal(t *testing.T) {
	fake := &fakeUpstream{models: []string{"m-one"}, inferenceFail: true,
		validKey: "sk-noinfer-000"}
	h := newHarness(t, fake.handler())
	acct := seededAccount(t, h)
	before := countTable(t, h, "provider_keys")

	code, resp := addKey(t, h, acct, map[string]any{
		"label": "k", "api_key": "sk-noinfer-000",
	})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("inference 401 must fail the test: %d %s", code, resp)
	}
	if e := errObj(t, resp); e["step"] != "inference" {
		t.Errorf("step = %v, want inference", e["step"])
	}
	if after := countTable(t, h, "provider_keys"); after != before {
		t.Errorf("failed inference must store nothing: keys %d → %d", before, after)
	}
}

func TestUnreachableUpstreamIsRetryable502(t *testing.T) {
	fake := &fakeUpstream{models: []string{"m"}}
	h := newHarness(t, fake.handler())
	acct := seededAccount(t, h)

	// Kill the upstream: every probe now fails with connection refused.
	h.upstream.Close()

	code, resp := addKey(t, h, acct, map[string]any{
		"label": "k", "api_key": "sk-whatever-000",
	})
	if code != http.StatusBadGateway {
		t.Fatalf("unreachable upstream must be 502, got %d: %s", code, resp)
	}
	if e := errObj(t, resp); e["retryable"] != true {
		t.Errorf("want retryable=true so the UI says 'try later', not 'your key is wrong': %v", e)
	}
}

// ── guards ──────────────────────────────────────────────────────────────────

func TestAdminRoleGuardsNewRoutes(t *testing.T) {
	fake := &fakeUpstream{models: []string{"m"}}
	h := newHarness(t, fake.handler())
	// infer-only token must be refused on every admin surface.
	for _, path := range []string{
		"/admin/accounts", "/admin/combos", "/admin/tokens",
		"/admin/overview", "/admin/export", "/admin/quota", "/admin/compression-profiles",
		"/admin/requests",
	} {
		code, _, _ := h.do("GET", path, "tok-infer", "")
		if code != http.StatusForbidden {
			t.Errorf("%s with infer-only token = %d, want 403", path, code)
		}
	}
	// Anonymous is 401, not 403 — the token itself is missing.
	if code, _, _ := h.do("GET", "/admin/accounts", "", ""); code != http.StatusUnauthorized {
		t.Errorf("no token = %d, want 401", code)
	}
	// Admin passes.
	if code, _, _ := h.do("GET", "/admin/accounts", "tok-admin", ""); code != 200 {
		t.Errorf("admin token on /admin/accounts = %d, want 200", code)
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	fake := &fakeUpstream{models: []string{"m"}}
	h := newHarness(t, fake.handler())
	code, resp := jsonDo(t, h, "POST", "/admin/accounts", "tok-admin", map[string]any{
		"name": "x", "namespace": "x", "kind": "openai-compatible",
		"base_url": "http://example.invalid", "enabled": true, "typo_field": 1,
	})
	if code != http.StatusBadRequest {
		t.Errorf("unknown body field must be rejected, got %d: %s", code, resp)
	}
}

func TestAccountEndpointPresetAndCompatibilityRequirement(t *testing.T) {
	h := newHarness(t, (&fakeUpstream{models: []string{"m"}}).handler())

	code, body := jsonDo(t, h, http.MethodPost, "/admin/accounts", "tok-admin", map[string]any{
		"name": "preset", "namespace": "preset", "kind": "opencode-go",
	})
	if code != http.StatusCreated {
		t.Fatalf("OpenCode account without URL = %d: %s", code, body)
	}
	var account map[string]any
	if err := json.Unmarshal([]byte(body), &account); err != nil {
		t.Fatal(err)
	}
	if account["base_url"] != "https://opencode.ai/zen/go/v1" {
		t.Fatalf("preset base_url = %v", account["base_url"])
	}

	code, body = jsonDo(t, h, http.MethodPost, "/admin/accounts", "tok-admin", map[string]any{
		"name": "compat", "namespace": "compat", "kind": "openai-compatible",
	})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("compat account without URL = %d, want 422: %s", code, body)
	}
}

func TestAccountDeleteGuard(t *testing.T) {
	fake := &fakeUpstream{models: []string{"m"}}
	h := newHarness(t, fake.handler())
	acct := seededAccount(t, h)

	// Block it with a combo hop — the "referenced by combo" conflict.
	code, resp := jsonDo(t, h, "POST", "/admin/combos", "tok-admin", map[string]any{
		"name": "c", "strategy": "failover",
		"hops": []any{map[string]any{"account_id": acct, "model_id": "up/gpt-6-luna", "weight": 1}},
	})
	if code != http.StatusCreated {
		t.Fatalf("combo create: %d %s", code, resp)
	}

	code, resp = jsonDo(t, h, "DELETE", fmt.Sprintf("/admin/accounts/%d", acct), "tok-admin", nil)
	if code != http.StatusConflict {
		t.Fatalf("deleting a referenced account must 409, got %d: %s", code, resp)
	}
	// the referents ride as `combos` + a human message — the UI renders both
	e := errObj(t, resp)
	combos, _ := e["combos"].([]any)
	if len(combos) == 0 {
		t.Errorf("409 must name the blocking combos so the UI can show them: %v", e)
	}
	if msg, _ := e["message"].(string); !strings.Contains(msg, "referenced") {
		t.Errorf("409 message should say what blocks it, got %q", msg)
	}

	code, resp = jsonDo(t, h, "DELETE", fmt.Sprintf("/admin/accounts/%d?force=true", acct),
		"tok-admin", nil)
	if code != http.StatusNoContent {
		t.Errorf("force delete: got %d %s", code, resp)
	}
}

func TestOverviewSingleRoundTrip(t *testing.T) {
	fake := &fakeUpstream{models: []string{"m-one", "m-two"}}
	h := newHarness(t, fake.handler())

	code, resp := jsonDo(t, h, "GET", "/admin/overview", "tok-admin", nil)
	if code != http.StatusOK {
		t.Fatalf("overview: %d %s", code, resp)
	}
	out := mustJSON(t, resp)
	for _, k := range []string{"health", "usage", "usage_by_surface", "accounts", "recent_calls"} {
		if out[k] == nil {
			t.Errorf("overview missing %q — the dashboard's first paint needs it", k)
		}
	}
	if strings.Contains(resp, seededSecret) || strings.Contains(resp, "tok-admin") {
		t.Errorf("overview leaked a secret")
	}
}

func TestTokenPlaintextShownOnce(t *testing.T) {
	fake := &fakeUpstream{models: []string{"m"}}
	h := newHarness(t, fake.handler())

	code, resp := jsonDo(t, h, "POST", "/admin/tokens", "tok-admin",
		map[string]any{"name": "opencode", "roles": []string{"infer"}})
	if code != http.StatusCreated {
		t.Fatalf("create token: %d %s", code, resp)
	}
	plain, _ := mustJSON(t, resp)["plaintext"].(string)
	if plain == "" {
		t.Fatalf("create token must return the plaintext exactly once: %s", resp)
	}
	_, listResp := jsonDo(t, h, "GET", "/admin/tokens", "tok-admin", nil)
	if strings.Contains(listResp, plain) {
		t.Errorf("GET /admin/tokens re-served the token plaintext")
	}
	code, resp = jsonDo(t, h, "POST", "/admin/tokens", "tok-admin",
		map[string]any{"name": "opencode", "roles": []string{"admin"}})
	if code != http.StatusConflict {
		t.Fatalf("duplicate token name should return 409 without replacing it, got %d: %s", code, resp)
	}
	if countTable(t, h, "client_tokens") != 3 { // seeded hermes + admin + first created token
		t.Fatalf("duplicate token attempt changed token row count")
	}
}

func TestExportHasNoCredentials(t *testing.T) {
	fake := &fakeUpstream{models: []string{"m"}}
	h := newHarness(t, fake.handler())

	code, hdr, resp := h.do("GET", "/admin/export", "tok-admin", "")
	if code != http.StatusOK {
		t.Fatalf("export: %d %s", code, resp)
	}
	if ct := hdr.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("export content-type = %q", ct)
	}
	if cd := hdr.Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("export should download as an attachment, got %q", cd)
	}
	// Hints are allowed by the contract; the verbatim stored credential is not.
	if strings.Contains(resp, seededSecret) {
		t.Errorf("export leaked the credential plaintext")
	}
	if strings.Contains(resp, "encrypted_api_key") {
		t.Errorf("export must not carry the encrypted key column at all")
	}
}

func TestComboHopsReplaceNotAppend(t *testing.T) {
	fake := &fakeUpstream{models: []string{"m"}}
	h := newHarness(t, fake.handler())
	acct := seededAccount(t, h)

	// two DISTINCT model_ids: position is the array order, assigned at insert time
	code, resp := jsonDo(t, h, "POST", "/admin/combos", "tok-admin", map[string]any{
		"name": "c", "strategy": "failover",
		"hops": []any{map[string]any{"account_id": acct, "model_id": "m-first", "weight": 1}},
	})
	if code != http.StatusCreated {
		t.Fatalf("combo create: %d %s", code, resp)
	}
	comboID := int64(mustJSON(t, resp)["id"].(float64))

	code, resp = jsonDo(t, h, "POST", fmt.Sprintf("/admin/combos/%d/hops", comboID),
		"tok-admin", map[string]any{"hops": []any{
			map[string]any{"account_id": acct, "model_id": "m-second", "weight": 2},
			map[string]any{"account_id": acct, "model_id": "m-third", "weight": 3},
		}})
	if code != http.StatusOK {
		t.Fatalf("hops replace: %d %s", code, resp)
	}
	got, _ := mustJSON(t, resp)["hops"].([]any)
	if len(got) != 2 {
		t.Fatalf("hops = %d, want 2 (replace must not accumulate)", len(got))
	}
	// order preserved = the replace wrote the list fresh, not appended
	if m := got[0].(map[string]any)["model_id"]; m != "m-second" {
		t.Errorf("hop[0] = %v, want m-second", m)
	}
	if m := got[1].(map[string]any)["model_id"]; m != "m-third" {
		t.Errorf("hop[1] = %v, want m-third", m)
	}
}

// Toggling a combo must be a one-field PATCH that takes effect in BOTH
// directions: the body's `enabled` wins (it used to be dropped on the floor, so
// a toggle silently did nothing), a body that omits it keeps the current value,
// and a disabled combo disappears from /v1/models.
func TestComboEnableToggle(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	acct := seededAccount(t, h)
	code, resp := jsonDo(t, h, "POST", "/admin/combos", "tok-admin", map[string]any{
		"name": "daily", "strategy": "failover",
		"hops": []any{map[string]any{"account_id": acct, "model_id": "gpt-6-luna", "weight": 1}},
	})
	if code != http.StatusCreated {
		t.Fatalf("create combo: %d %s", code, resp)
	}
	created := mustJSON(t, resp)
	id := int64(created["id"].(float64))
	if !created["enabled"].(bool) {
		t.Fatalf("a combo created without `enabled` must default to enabled: %s", resp)
	}

	listed := func() bool {
		t.Helper()
		_, _, body := h.do("GET", "/v1/models", "tok-infer", "")
		return strings.Contains(body, `"daily"`)
	}
	if !listed() {
		t.Fatalf("/v1/models should advertise an enabled combo")
	}
	_, _, modelsBody := h.do("GET", "/v1/models", "tok-infer", "")
	var models struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(modelsBody), &models); err != nil {
		t.Fatal(err)
	}
	foundContext := false
	for _, model := range models.Data {
		if model["id"] == "daily" {
			foundContext = model["context_size"] == float64(200000)
		}
	}
	if !foundContext {
		t.Fatalf("/v1/models must advertise combo context_size=200000: %s", modelsBody)
	}
	if code, resp = jsonDo(t, h, "PATCH", fmt.Sprintf("/admin/combos/%d", id), "tok-admin",
		map[string]any{"context_size": 128000}); code != http.StatusOK {
		t.Fatalf("patch context_size: %d %s", code, resp)
	}
	if got := mustJSON(t, resp)["context_size"]; got != float64(128000) {
		t.Fatalf("updated context_size = %v, want 128000", got)
	}
	if code, resp = jsonDo(t, h, "PATCH", fmt.Sprintf("/admin/combos/%d", id), "tok-admin",
		map[string]any{"notes": "preserve context"}); code != http.StatusOK || mustJSON(t, resp)["context_size"] != float64(128000) {
		t.Fatalf("omitted context_size must stay unchanged: %d %s", code, resp)
	}

	// body toggle off
	if code, resp = jsonDo(t, h, "PATCH", fmt.Sprintf("/admin/combos/%d", id), "tok-admin",
		map[string]any{"enabled": false}); code != http.StatusOK {
		t.Fatalf("patch enabled=false: %d %s", code, resp)
	}
	if mustJSON(t, resp)["enabled"].(bool) {
		t.Fatalf(`PATCH {"enabled":false} must disable the combo: %s`, resp)
	}
	if listed() {
		t.Errorf("a disabled combo must not be advertised in /v1/models")
	}

	// an unrelated patch must leave it off (absent = unchanged)
	if code, resp = jsonDo(t, h, "PATCH", fmt.Sprintf("/admin/combos/%d", id), "tok-admin",
		map[string]any{"notes": "parked"}); code != http.StatusOK {
		t.Fatalf("patch notes: %d %s", code, resp)
	}
	if mustJSON(t, resp)["enabled"].(bool) {
		t.Fatalf("a patch that omits `enabled` must leave it disabled: %s", resp)
	}

	// ... and the query form still works for existing callers
	if code, resp = jsonDo(t, h, "PATCH", fmt.Sprintf("/admin/combos/%d?enabled=true", id), "tok-admin",
		map[string]any{}); code != http.StatusOK {
		t.Fatalf("patch ?enabled=true: %d %s", code, resp)
	}
	if !mustJSON(t, resp)["enabled"].(bool) {
		t.Fatalf("?enabled=true must re-enable the combo: %s", resp)
	}
	if !listed() {
		t.Errorf("a re-enabled combo must be back in /v1/models")
	}
}

func TestComboHopUnknownAccount(t *testing.T) {
	fake := &fakeUpstream{models: []string{"m"}}
	h := newHarness(t, fake.handler())
	code, resp := jsonDo(t, h, "POST", "/admin/combos", "tok-admin", map[string]any{
		"name": "bad", "strategy": "failover",
		"hops": []any{map[string]any{"account_id": 99999, "model_id": "x", "weight": 1}},
	})
	if code != http.StatusUnprocessableEntity {
		t.Errorf("hop with unknown account must 422, got %d: %s", code, resp)
	}
}

// ── the retest contract ─────────────────────────────────────────────────────

func TestRetestFailureKeepsTheRow(t *testing.T) {
	fake := &fakeUpstream{models: []string{"m"}, validKey: "sk-rt-000"}
	h := newHarness(t, fake.handler())
	acct := seededAccount(t, h)

	code, resp := addKey(t, h, acct, map[string]any{
		"label": "rt", "api_key": "sk-rt-000", "skip_inference": true,
	})
	if code != http.StatusCreated {
		t.Fatalf("add key: %d %s", code, resp)
	}
	keyID := int64(mustJSON(t, resp)["key"].(map[string]any)["id"].(float64))

	// The upstream starts rejecting the key — a world change, not our fault.
	fake.authFail = true

	code, resp = jsonDo(t, h, "POST",
		fmt.Sprintf("/admin/accounts/%d/keys/%d/retest", acct, keyID), "tok-admin", map[string]any{})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("retest against a now-bad upstream: %d %s", code, resp)
	}
	if n := countTable(t, h, "provider_keys"); n != 2 {
		t.Errorf("a failed RETEST must keep the row (it was valid once), got %d keys", n)
	}
	var ok *int
	if err := h.db.Reader().QueryRow(
		`SELECT last_test_ok FROM provider_keys WHERE label='rt'`).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	if ok == nil || *ok != 0 {
		t.Errorf("last_test_ok should be 0 after a failed retest, got %v", ok)
	}
}
