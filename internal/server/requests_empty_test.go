package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/store"
)

// Successful rows with no tokens recorded (bridge tool steps) are counted in
// the summary and can be hidden; failed zero-token rows are never "steps".
func TestRequestsCountAndHideZeroTokenSteps(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	now := time.Now()
	for _, c := range []store.Call{
		{TS: now, Client: "my-app", Surface: store.SurfaceChat, Model: "m", Status: 200, TokensIn: 10, TokensOut: 5},
		{TS: now, Client: "my-app", Surface: store.SurfaceChat, Model: "m", Status: 200, TokensCachedRead: 900},
		{TS: now, Client: "my-app", Surface: store.SurfaceChat, Model: "m", Status: 200},
		{TS: now, Client: "my-app", Surface: store.SurfaceChat, Model: "m", Status: 200},
		{TS: now, Client: "my-app", Surface: store.SurfaceChat, Model: "m", Status: 502},
	} {
		h.db.RecordCall(c)
	}
	if err := h.db.Flush(); err != nil {
		t.Fatal(err)
	}
	code, body := jsonDo(t, h, "GET", "/admin/requests", "tok-admin", nil)
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, body)
	}
	all := decodeEmpty(t, body)
	if all.Summary.Count != 5 || all.Summary.EmptySteps != 2 {
		t.Fatalf("count=%d empty_steps=%d, want 5 and 2", all.Summary.Count, all.Summary.EmptySteps)
	}
	code, body = jsonDo(t, h, "GET", "/admin/requests?hide_empty=1", "tok-admin", nil)
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, body)
	}
	hidden := decodeEmpty(t, body)
	if hidden.Summary.Count != 3 || hidden.Summary.EmptySteps != 0 || len(hidden.Rows) != 3 || !hidden.Filter.HideEmpty {
		t.Fatalf("hide_empty: count=%d empty_steps=%d rows=%d filter=%v", hidden.Summary.Count, hidden.Summary.EmptySteps, len(hidden.Rows), hidden.Filter.HideEmpty)
	}
	if code, _ := jsonDo(t, h, "GET", "/admin/requests?hide_empty=maybe", "tok-admin", nil); code != http.StatusBadRequest {
		t.Fatalf("invalid hide_empty accepted: %d", code)
	}
}

type emptyStepsResp struct {
	Rows    []map[string]any `json:"rows"`
	Summary struct {
		Count      int64 `json:"count"`
		EmptySteps int64 `json:"empty_steps"`
	} `json:"summary"`
	Filter struct {
		HideEmpty bool `json:"hide_empty"`
	} `json:"filter"`
}

func decodeEmpty(t *testing.T, body string) emptyStepsResp {
	t.Helper()
	var out emptyStepsResp
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("body is not JSON: %s", body)
	}
	return out
}
