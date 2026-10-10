package server

// v8 admin surface: prices + cost. The endpoint tests below pin the contract
// the dashboard will rely on — model id in the BODY (ids can contain "/"),
// all-four-rates-required (a missing rate must not silently mean "free"),
// unknown model 404, and cost's honest unpriced accounting.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

func pricesPath(acct int64) string { return fmt.Sprintf("/admin/accounts/%d/prices", acct) }

func TestPricesAPI(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	acct := seededAccountID(t, h)

	// missing model
	if code, _, _ := h.do("PUT", pricesPath(acct), adminTok(), `{"effective_from":"2026-01-01"}`); code != 400 {
		t.Errorf("missing model: %d, want 400", code)
	}
	// missing effective_from
	if code, _, _ := h.do("PUT", pricesPath(acct), adminTok(), `{"model":"gpt-6-luna","price_in":1}`); code != 400 {
		t.Errorf("missing date: %d, want 400", code)
	}
	// partial rates: price_out etc absent must NOT be treated as zero/free
	partial := `{"model":"gpt-6-luna","effective_from":"2026-01-01","price_in":3}`
	if code, _, _ := h.do("PUT", pricesPath(acct), adminTok(), partial); code != 400 {
		t.Errorf("partial rates: %d, want 400", code)
	}
	// unknown model -> 404 (never create a floating rate)
	unknown := `{"model":"ghost/m","effective_from":"2026-01-01","price_in":1,"price_out":1,"price_cache_read":0,"price_cache_write":0}`
	if code, _, _ := h.do("PUT", pricesPath(acct), adminTok(), unknown); code != 404 {
		t.Errorf("unknown model: %d, want 404", code)
	}
	// valid set -> 200, echo normalized
	full := `{"model":"gpt-6-luna","effective_from":"2026-01-01T00:00:00Z","price_in":2.5,"price_out":10,"price_cache_read":1.25,"price_cache_write":0,"note":"test"}`
	code, _, body := h.do("PUT", pricesPath(acct), adminTok(), full)
	if code != 200 {
		t.Fatalf("valid set: %d (%s)", code, body)
	}
	var echoed struct {
		EffectiveFrom string  `json:"effective_from"`
		Currency      string  `json:"currency"`
		PriceIn       float64 `json:"price_in"`
	}
	if err := json.Unmarshal([]byte(body), &echoed); err != nil {
		t.Fatalf("echo not json: %v", err)
	}
	if echoed.EffectiveFrom != "2026-01-01" || echoed.Currency != "USD" || echoed.PriceIn != 2.5 {
		t.Errorf("echo = %+v, want normalized date/USD/2.5", echoed)
	}
	// list shows it
	code, _, body = h.do("GET", "/admin/prices", adminTok(), "")
	if code != 200 {
		t.Fatalf("list: %d", code)
	}
	var list []map[string]any
	if err := json.Unmarshal([]byte(body), &list); err != nil || len(list) != 1 {
		t.Fatalf("list body = %s err %v", body, err)
	}
	// cost window covering the seeded traffic
	h.do("POST", "/v1/chat/completions", "tok-infer", `{"model":"up/gpt-6-luna","messages":[{"role":"user","content":"hi"}]}`)
	if err := h.db.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	code, _, body = h.do("GET", "/admin/cost?from=2020-01-01T00:00:00Z&to=2099-01-01T00:00:00Z", adminTok(), "")
	if code != 200 {
		t.Fatalf("cost: %d (%s)", code, body)
	}
	var cost struct {
		Totals struct {
			USD      float64 `json:"usd"`
			Calls    int64   `json:"calls"`
			Unpriced int64   `json:"unpriced_calls"`
			Complete bool    `json:"pricing_complete"`
		} `json:"totals"`
		Rows []struct {
			Model string  `json:"model"`
			USD   float64 `json:"usd"`
		} `json:"rows"`
	}
	if err := json.Unmarshal([]byte(body), &cost); err != nil {
		t.Fatalf("cost json: %v (%s)", err, body)
	}
	// The successful upstream call above served gpt-6-luna (priced 2.5/10 from
	// 2026-01-01). Any other rows (e.g. the 422-key test's alias) may exist;
	// we only assert the invariant: usd >= 0 and pricing_complete says the
	// truth about unpriced calls.
	if cost.Totals.USD < 0 {
		t.Errorf("negative usd: %v", cost.Totals.USD)
	}
	if cost.Totals.Unpriced > 0 && cost.Totals.Complete {
		t.Error("pricing_complete true while unpriced_calls > 0")
	}
	// bad range params -> 400
	if code, _, _ := h.do("GET", "/admin/cost?from=not-a-date", adminTok(), ""); code != 400 {
		t.Errorf("bad from: %d, want 400", code)
	}
	// delete wrong version -> 404, right version -> 204
	delBad := `{"model":"gpt-6-luna","effective_from":"2020-01-01"}`
	if code, _, _ := h.do("DELETE", pricesPath(acct), adminTok(), delBad); code != 404 {
		t.Errorf("delete wrong version: %d, want 404", code)
	}
	delOK := `{"model":"gpt-6-luna","effective_from":"2026-01-01"}`
	if code, _, _ := h.do("DELETE", pricesPath(acct), adminTok(), delOK); code != 204 {
		t.Errorf("delete exact version: %d, want 204", code)
	}
	// no auth -> 401 everywhere
	for _, m := range []struct{ method, path, body string }{
		{"GET", "/admin/prices", ""},
		{"PUT", pricesPath(acct), full},
		{"GET", "/admin/cost", ""},
	} {
		if code, _, _ := h.do(m.method, m.path, "", m.body); code != 401 {
			t.Errorf("%s %s unauthenticated: %d, want 401", m.method, m.path, code)
		}
	}
}

// TestShapeColumnsSurfaceInRequests proves /admin/rows expose the v8 capture:
// a call recorded through the real path must carry prefix_sha and req_bytes in
// the requests projection (otherwise the whole capture pipeline is invisible).
func TestShapeColumnsSurfaceInRequests(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"message":{"content":"OK"}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`))
	})
	req := `{"model":"up/gpt-6-luna","messages":[{"role":"system","content":"s"},{"role":"user","content":"hi"}]}`
	h.do("POST", "/v1/chat/completions", "tok-infer", req)
	// A second identical-context call (sibling reuse) with a session header.
	h.do("POST", "/v1/chat/completions", "tok-infer", req)
	// The ledger is async + batched; flush so the rows are visible to the read.
	if err := h.db.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	code, _, body := h.do("GET", "/admin/requests?limit=10", adminTok(), "")
	if code != 200 {
		t.Fatalf("requests: %d", code)
	}
	var out struct {
		Rows []struct {
			PrefixSHA string `json:"prefix_sha"`
			MsgCount  int    `json:"msg_count"`
			ReqBytes  int    `json:"req_bytes"`
			SessionID string `json:"session_id"`
			Saved     int    `json:"saved"`
			SavedNotl int    `json:"saved_notional"`
		} `json:"rows"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("json: %v (%s)", err, body)
	}
	if len(out.Rows) < 2 {
		t.Fatalf("rows = %d, want >= 2", len(out.Rows))
	}
	a, b := out.Rows[0], out.Rows[1]
	if a.PrefixSHA == "" || a.ReqBytes == 0 || a.MsgCount != 2 {
		t.Errorf("row missing shape capture: %+v", a)
	}
	if a.PrefixSHA != b.PrefixSHA {
		t.Errorf("identical contexts should share prefix_sha in the API too")
	}
	// saved columns must be present and non-negative (0 with no compression).
	if a.Saved < 0 || a.SavedNotl < 0 {
		t.Errorf("saved columns negative: %+v", a)
	}
}
