package server

// Two operator-visibility features proven together, because they answer the
// same question from the same ledger rows:
//
//   - block_pos: "N requests finished" — each row says which dispatch of its
//     block it was, so a run longer than N is VISIBLY a fallback, not a
//     counting bug.
//   - the failover trail: hops that failed or were skipped leave no row of
//     their own (one request = one row, the billing truth), so the served
//     row's err carries them.
//
// The harness's "bad" upstream answers 429 to chat, so a block that rotates
// onto it must fall over to "good" — and that row must show BOTH the trail
// and the block position it consumed.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/store"
)

func TestBlockPosAndFailoverTrailInLedger(t *testing.T) {
	tu := newTwoUpstream(t, true)
	ctx := context.Background()

	// Find the harness's account ids by namespace, and build a 2-hop block
	// combo: good first (so it serves), bad second (429 → failover).
	accounts, err := tu.db.ListAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]int64{}
	for _, a := range accounts {
		ids[a.Namespace] = a.ID
	}
	if ids["good"] == 0 || ids["bad"] == 0 {
		t.Fatalf("harness accounts missing: %v", ids)
	}
	if _, err := tu.db.UpsertCombo(ctx, store.Combo{
		Name: "blk", Strategy: "block_round_robin", BlockSize: 2, Enabled: true,
		Hops: []store.ComboHop{
			{AccountID: ids["good"], ModelID: "gpt-6-luna", Weight: 1},
			{AccountID: ids["bad"], ModelID: "gpt-6-luna", Weight: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}

	body := `{"model":"blk","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`
	for i := 0; i < 3; i++ {
		if code, resp := tu.chat(body); code != http.StatusOK {
			t.Fatalf("dispatch %d: status %d body %s", i+1, code, resp)
		}
	}

	// The ledger is written asynchronously — poll for the third row.
	deadline := time.Now().Add(3 * time.Second)
	var rows []map[string]any
	for {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/admin/requests?limit=20", nil)
		req.Header.Set("Authorization", "Bearer tok-admin")
		tu.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("requests: %d %s", rec.Code, rec.Body.String())
		}
		var payload struct {
			Rows []map[string]any `json:"rows"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		rows = rows[:0]
		for _, r := range payload.Rows {
			if a, _ := r["alias"].(string); a == "blk" {
				rows = append(rows, r)
			}
		}
		if len(rows) >= 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d blk rows, want 3", len(rows))
	}

	// Newest first: dispatch 3 fell over (pos 1 of its new block), 2 and 1 were
	// the first block's two slots.
	wantPos := []float64{1, 2, 1}
	for i, r := range rows {
		pos, _ := r["block_pos"].(float64)
		if pos != wantPos[i] {
			t.Errorf("row %d block_pos = %v, want %v (rows: %v)", i, pos, wantPos[i], summarize(rows))
		}
		if acct, _ := r["account"].(string); acct != "good" {
			t.Errorf("row %d served by %q, want good (the 429 hop must fall over)", i, acct)
		}
	}

	// Row 0 fell over from the bad hop: the trail names it, with the
	// classifier's cause and the upstream's own words.
	errText, _ := rows[0]["err"].(string)
	if !strings.Contains(errText, "failover: bad/gpt-6-luna") {
		t.Errorf("served row lost the failover trail: %q", errText)
	}
	if !strings.Contains(errText, "[quota_exhausted]") {
		t.Errorf("trail missing the classified cause: %q", errText)
	}
	// The first two dispatches never touched the bad hop: no trail.
	for _, i := range []int{1, 2} {
		if e, _ := rows[i]["err"].(string); strings.Contains(e, "failover:") {
			t.Errorf("row %d unexpectedly carries a failure trail: %q", i, e)
		}
	}
}

func summarize(rows []map[string]any) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, strings.Join([]string{
			"account=" + r["account"].(string),
			"pos=" + jsonNum(r["block_pos"]),
			"err=" + trunc(r["err"]),
		}, " "))
	}
	return out
}

func jsonNum(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func trunc(v any) string {
	s, _ := v.(string)
	if len(s) > 60 {
		return s[:60] + "…"
	}
	return s
}
