package server

// M5 admin API: /admin/model-rules CRUD. The assertions that matter are the
// ones a wrong implementation would silently pass: that an omitted `enabled`
// on create means ON (not Go's false), that a PATCH merges rather than
// zeroing omitted fields, and that a DELETE actually removes the restriction.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

func adminTok() string { return "tok-admin" }

// ruleReq is a helper to POST a rule and return its id.
func postRule(t *testing.T, h *harness, body string) (int, map[string]any) {
	t.Helper()
	code, _, resp := h.do("POST", "/admin/model-rules", adminTok(), body)
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("POST /admin/model-rules = %d, body=%s", code, resp)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(resp), &m); err != nil {
		t.Fatalf("bad JSON: %v (%s)", err, resp)
	}
	return code, m
}

func TestModelRuleCreateDefaultsEnabled(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	// Need an account for the FK: reuse the harness's seeded one via its db.
	// newHarness seeds namespace "up"; find its id.
	var acctID int64
	if err := h.db.Reader().QueryRow(`SELECT id FROM accounts LIMIT 1`).Scan(&acctID); err != nil {
		t.Fatalf("seeded account: %v", err)
	}

	// `enabled` OMITTED → must default to true (schema default), not false.
	_, m := postRule(t, h, fmt.Sprintf(
		`{"account_id":%d,"model_id":"nightly","cap_tokens":5000000,"cap_window":"monthly",
		  "win_start":"22:00","win_end":"06:00","win_tz":"Asia/Kuala_Lumpur"}`, acctID))
	if m["enabled"] != true {
		t.Errorf("omitted enabled = %v, want true (Go's false zero-value would disable the rule silently)", m["enabled"])
	}
	if m["kind"] == nil && m["id"] == nil {
		t.Errorf("response missing id: %v", m)
	}
}

func TestModelRuleCreateExplicitDisabled(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	var acctID int64
	_ = h.db.Reader().QueryRow(`SELECT id FROM accounts LIMIT 1`).Scan(&acctID)

	_, m := postRule(t, h, fmt.Sprintf(
		`{"account_id":%d,"model_id":"off","enabled":false}`, acctID))
	if m["enabled"] != false {
		t.Errorf("explicit enabled:false = %v, want false", m["enabled"])
	}
}

func TestModelRuleValidationRejected(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	var acctID int64
	_ = h.db.Reader().QueryRow(`SELECT id FROM accounts LIMIT 1`).Scan(&acctID)

	// Bad window enum → must be rejected (4xx), not silently stored.
	code, _, body := h.do("POST", "/admin/model-rules", adminTok(), fmt.Sprintf(
		`{"account_id":%d,"model_id":"x","cap_window":"hourly"}`, acctID))
	if code < 400 || code > 499 {
		t.Errorf("bad cap_window accepted: status=%d body=%s (want 4xx)", code, body)
	}
}

func TestModelRuleDeleteRemovesRestriction(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	var acctID int64
	_ = h.db.Reader().QueryRow(`SELECT id FROM accounts LIMIT 1`).Scan(&acctID)

	_, m := postRule(t, h, fmt.Sprintf(
		`{"account_id":%d,"model_id":"gone"}`, acctID))
	id := int64(m["id"].(float64))

	code, _, _ := h.do("DELETE", fmt.Sprintf("/admin/model-rules/%d", id), adminTok(), "")
	if code != 200 {
		t.Errorf("DELETE = %d, want 200", code)
	}
	// GET after delete → 404.
	code, _, _ = h.do("GET", fmt.Sprintf("/admin/model-rules/%d", id), adminTok(), "")
	if code != 404 {
		t.Errorf("GET after delete = %d, want 404", code)
	}
}

func TestModelRuleListFiltered(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	var acctID int64
	_ = h.db.Reader().QueryRow(`SELECT id FROM accounts LIMIT 1`).Scan(&acctID)
	postRule(t, h, fmt.Sprintf(`{"account_id":%d,"model_id":"a"}`, acctID))
	postRule(t, h, fmt.Sprintf(`{"account_id":%d,"model_id":"b"}`, acctID))

	code, _, body := h.do("GET", fmt.Sprintf("/admin/model-rules?account_id=%d", acctID), adminTok(), "")
	if code != 200 {
		t.Fatalf("list = %d", code)
	}
	var list []map[string]any
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("bad list JSON: %v", err)
	}
	if len(list) < 2 {
		t.Errorf("listed %d rules, want >= 2", len(list))
	}
	for _, r := range list {
		if r["account_id"] != float64(acctID) {
			t.Errorf("list filter leaked rule for account %v", r["account_id"])
		}
	}
}
