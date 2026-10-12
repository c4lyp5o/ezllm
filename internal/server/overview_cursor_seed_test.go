package server

// The recent-calls table joins its rows to a combo's block cursor by NAME to
// render "▮ pos/size". The live cursor map is in-process, so a restart (or a
// combo that simply has not dispatched yet) left the table showing a bare
// "▮ 1" — the size lived only in the live map. The overview now seeds the size
// from the combo row, and a live cursor still supersedes the seed.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/store"
)

type overviewCursors struct {
	BlockCursors []struct {
		Combo     string `json:"combo"`
		Hop       string `json:"hop"`
		Remaining int    `json:"remaining"`
		BlockSize int    `json:"block_size"`
		Reason    string `json:"reason"`
	} `json:"block_cursors"`
}

func fetchCursors(t *testing.T, h *harness) overviewCursors {
	t.Helper()
	code, _, body := h.do("GET", "/admin/overview", adminTok(), "")
	if code != 200 {
		t.Fatalf("overview: %d (%s)", code, body)
	}
	var ov overviewCursors
	if err := json.Unmarshal([]byte(body), &ov); err != nil {
		t.Fatal(err)
	}
	return ov
}

func TestOverviewSeedsBlockSizeWithoutLiveCursor(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	})
	acct := seededAccountID(t, h)
	const name = "seed-block-proof"

	if _, err := h.db.UpsertCombo(context.Background(), store.Combo{
		Name: name, Strategy: "block_round_robin", Enabled: true, BlockSize: 5,
		Hops: []store.ComboHop{{AccountID: acct, ModelID: "gpt-6-luna", Weight: 1}},
	}); err != nil {
		t.Fatal(err)
	}

	// No dispatch yet: the size must already be readable, or rows whose
	// block_pos came from the ledger render as a bare "▮ 1".
	seeded := false
	for _, c := range fetchCursors(t, h).BlockCursors {
		if c.Combo != name {
			continue
		}
		seeded = true
		if c.BlockSize != 5 {
			t.Errorf("seeded block_size = %d, want 5", c.BlockSize)
		}
		if c.Hop != "" {
			t.Errorf("seeded hop = %q, want empty (no live block yet)", c.Hop)
		}
		if c.Reason != "cold start" {
			t.Errorf("seeded reason = %q, want cold start", c.Reason)
		}
	}
	if !seeded {
		t.Fatalf("overview has no cursor for the enabled block combo %q", name)
	}

	// After a real dispatch the live cursor takes over — and must NOT be
	// listed twice.
	code, _, body := h.do("POST", "/v1/chat/completions", "tok-infer",
		`{"model":"`+name+`","messages":[{"role":"user","content":"ping"}]}`)
	if code != 200 {
		t.Fatalf("dispatch through the block combo: %d (%s)", code, body)
	}
	count, hop, reason := 0, "", ""
	for _, c := range fetchCursors(t, h).BlockCursors {
		if c.Combo == name {
			count++
			hop, reason = c.Hop, c.Reason
		}
	}
	if count != 1 {
		t.Fatalf("cursor entries for %q = %d, want exactly 1 (seed superseded by the live cursor)", name, count)
	}
	if !strings.HasSuffix(hop, "/gpt-6-luna") {
		t.Errorf("live cursor hop = %q, want the hop that served", hop)
	}
	if reason != "cold start" {
		t.Errorf("live cursor reason = %q, want cold start (first dispatch of the process)", reason)
	}
}
