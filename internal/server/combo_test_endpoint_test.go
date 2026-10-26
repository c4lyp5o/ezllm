package server

// The combo-test endpoint must probe EVERY hop with the REAL dispatcher and
// the results must be truthful enough to trust: the failing hop earns a real
// status with a classified cause, the healthy hop earns 200, and the probes
// themselves stay out of the ledger.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestComboTestProbesEveryHopAndFillsErr(t *testing.T) {
	tu := newTwoUpstream(t, true) // bad upstream 429s chat, good 200s
	tu.badHits, tu.goodHits = nil, nil

	// Find the combo id (the harness created "daily").
	list := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/admin/combos", nil)
	req.Header.Set("Authorization", "Bearer tok-admin")
	tu.handler.ServeHTTP(list, req)
	if list.Code != http.StatusOK {
		t.Fatalf("list combos: %d %s", list.Code, list.Body.String())
	}
	var combos []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &combos); err != nil {
		t.Fatal(err)
	}
	var comboID int64
	for _, c := range combos {
		if c.Name == "daily" {
			comboID = c.ID
		}
	}
	if comboID == 0 {
		t.Fatal("daily combo not found")
	}

	// Run the test as the ADMIN (it is an admin surface), surface defaults to openai.
	rec := httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/admin/combos/"+itoa(comboID)+"/test", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer tok-admin")
	req.Header.Set("Content-Type", "application/json")
	tu.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("combo test: %d %s", rec.Code, rec.Body.String())
	}

	var out struct {
		Combo   string `json:"combo"`
		Surface string `json:"surface"`
		Hops    []struct {
			Namespace string `json:"namespace"`
			Model     string `json:"model"`
			Enabled   bool   `json:"enabled"`
			Status    int    `json:"status"`
			Err       string `json:"err"`
			Cause     string `json:"cause"`
			Parked    bool   `json:"parked"`
		} `json:"hops"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Combo != "daily" || out.Surface != "openai" {
		t.Fatalf("combo/surface = %q/%q", out.Combo, out.Surface)
	}
	if len(out.Hops) != 2 {
		t.Fatalf("hops = %d, want 2 (every hop probed, none filtered)", len(out.Hops))
	}

	bad, good := out.Hops[0], out.Hops[1]
	if bad.Namespace != "bad" || good.Namespace != "good" {
		t.Fatalf("hop order = %s,%s — combo position order must survive", bad.Namespace, good.Namespace)
	}
	if bad.Status != http.StatusTooManyRequests {
		t.Errorf("bad hop status = %d, want 429", bad.Status)
	}
	if bad.Cause != "quota_exhausted" {
		t.Errorf("bad hop cause = %q, want quota_exhausted (from the provider's own words)", bad.Cause)
	}
	if !strings.Contains(bad.Err, "quota exhausted") {
		t.Errorf("bad hop err lost the upstream words: %q", bad.Err)
	}
	if good.Status != http.StatusOK {
		t.Errorf("good hop status = %d, want 200", good.Status)
	}
	if good.Err != "" {
		t.Errorf("good hop err = %q, want empty", good.Err)
	}
	if len(tu.badHits) != 1 || len(tu.goodHits) != 1 {
		t.Fatalf("upstream hits = bad %d / good %d, want exactly one probe each (zero retries)", len(tu.badHits), len(tu.goodHits))
	}

	// The probes go through the real dispatcher but are admin traffic: none of
	// them may reach the ledger the recent-calls table reads.
	time.Sleep(100 * time.Millisecond) // the ledger batches asynchronously (20ms here)
	calls := httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/admin/requests?limit=10", nil)
	req.Header.Set("Authorization", "Bearer tok-admin")
	tu.handler.ServeHTTP(calls, req)
	if calls.Code != http.StatusOK {
		t.Fatalf("requests: %d %s", calls.Code, calls.Body.String())
	}
	var payload struct {
		Rows []map[string]any `json:"rows"`
	}
	if err := json.Unmarshal(calls.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Rows) != 0 {
		t.Fatalf("combo-test probes reached the ledger: %v", payload.Rows)
	}

	// Overview carries the cursor list (empty here: no block combo ran) so
	// the dashboard's recent-calls table has the field it joins on.
	ov := httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/admin/overview", nil)
	req.Header.Set("Authorization", "Bearer tok-admin")
	tu.handler.ServeHTTP(ov, req)
	var ovj map[string]any
	if err := json.Unmarshal(ov.Body.Bytes(), &ovj); err != nil {
		t.Fatal(err)
	}
	if _, ok := ovj["block_cursors"]; !ok {
		t.Error("overview is missing block_cursors")
	}
}
