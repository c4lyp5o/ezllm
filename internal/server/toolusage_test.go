package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/compress"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// toolPruningBody posts a chat completion with a two-tool action space and a
// trace that has already called Live. Ghost is the never-called tool.
const toolPruningBody = `{"model":"up/gpt-6-luna","max_tokens":50,
  "tools":[
    {"type":"function","function":{"name":"Ghost","description":"never called"}},
    {"type":"function","function":{"name":"Live","description":"called"}}
  ],
  "messages":[
    {"role":"user","content":"do things"},
    {"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"Live","arguments":"{}"}}]},
    {"role":"tool","tool_call_id":"c1","content":"ok"}
  ]}`

// ghostTokens is the documented estimator (compress.EstimateTokens over the
// marshalled entry) — the exact number the shadow column must carry.
func ghostTokens(t *testing.T) int64 {
	t.Helper()
	var doc struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal([]byte(toolPruningBody), &doc); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(doc.Tools[0])
	if err != nil {
		t.Fatal(err)
	}
	return compress.EstimateTokens(string(b))
}

func TestToolPruneShadowBooksAndCollects(t *testing.T) {
	var mu sync.Mutex
	var upstreamBody []byte
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		upstreamBody, _ = io.ReadAll(r.Body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	}, func(o *Options) {
		o.ToolPruneMode = "shadow"
		o.ToolPruneMinUnusedSessions = 3
	})
	// Pre-request stats make Ghost policy-eligible: shipped in 3 sessions,
	// called in none. Live must stay ineligible (used_sessions > 0).
	if err := h.db.UpsertToolUsage(context.Background(), []store.ToolUsageDelta{
		{Client: "hermes", Tool: "Ghost", SchemaTokens: 99, ShippedRequests: 7, ShippedSessions: 3},
		{Client: "hermes", Tool: "Live", SchemaTokens: 99, ShippedRequests: 7, ShippedSessions: 3, UsedSessions: 1, LastUsedAt: "2026-10-30T00:00:00.000Z"},
	}); err != nil {
		t.Fatal(err)
	}

	code, _, body := h.do("POST", "/v1/chat/completions", "tok-infer", toolPruningBody)
	if code != 200 {
		t.Fatalf("code=%d body=%s", code, body)
	}
	if err := h.db.Flush(); err != nil {
		t.Fatal(err)
	}
	rows, err := h.db.RecentCalls(context.Background(), 5)
	if err != nil || len(rows) == 0 {
		t.Fatalf("ledger rows: %v %v", rows, err)
	}
	want := ghostTokens(t)
	if got := rows[0].ToolPruneShadowTokens; got != want {
		t.Errorf("tool_prune_shadow_tokens = %d, want %d (Ghost eligible, Live used)", got, want)
	}

	// Stats landed: Ghost kept its request counter and crossed a session;
	// Live flipped nothing new (its used verdict was already sticky).
	stats, err := h.db.ToolUsageForClient(context.Background(), "hermes")
	if err != nil {
		t.Fatal(err)
	}
	if g := stats["Ghost"]; g.ShippedRequests != 8 || g.ShippedSessions != 4 || g.UsedSessions != 0 {
		t.Errorf("Ghost stats = %+v, want req 8 / sess 4 / used 0", g)
	}
	// Live's used flip counts THIS session's pair on top of the seeded past
	// (the documented tracker behaviour: one pair per live session), and the
	// stamp moves — while eligibility stays sticky-ineligible (the booking
	// assertion above already proves it was never counted).
	if l := stats["Live"]; l.UsedSessions != 2 || l.LastUsedAt == "2026-10-30T00:00:00.000Z" {
		t.Errorf("Live stats = %+v, want used 2 with a moved stamp", l)
	}

	// M1 must NOT rewrite the body: the upstream sees both schemas.
	mu.Lock()
	got := upstreamBody
	mu.Unlock()
	var sent struct {
		Tools []any `json:"tools"`
	}
	if err := json.Unmarshal(got, &sent); err != nil {
		t.Fatalf("upstream body decode: %v", err)
	}
	if len(sent.Tools) != 2 {
		t.Errorf("upstream saw %d tools, want 2 — M1 never rewrites", len(sent.Tools))
	}
}

func TestToolPruneOffIsANoop(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	}) // zero Options: mode "" behaves as off
	code, _, body := h.do("POST", "/v1/chat/completions", "tok-infer", toolPruningBody)
	if code != 200 {
		t.Fatalf("code=%d body=%s", code, body)
	}
	if err := h.db.Flush(); err != nil {
		t.Fatal(err)
	}
	rows, err := h.db.RecentCalls(context.Background(), 5)
	if err != nil || len(rows) == 0 {
		t.Fatalf("ledger rows: %v %v", rows, err)
	}
	if rows[0].ToolPruneShadowTokens != 0 {
		t.Errorf("off mode must book nothing, got %d", rows[0].ToolPruneShadowTokens)
	}
	var n int
	if err := h.db.Reader().QueryRow(`SELECT COUNT(*) FROM tool_usage`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("off mode must collect no stats, got %d rows", n)
	}
}

func TestHandleToolUsage(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	}, func(o *Options) {
		o.ToolPruneMode = "shadow"
		o.ToolPruneMinUnusedSessions = 3
	})
	if err := h.db.UpsertToolUsage(context.Background(), []store.ToolUsageDelta{
		{Client: "hermes", Tool: "Ghost", SchemaTokens: 10, ShippedRequests: 4, ShippedSessions: 3},
		{Client: "hermes", Tool: "Live", SchemaTokens: 10, ShippedRequests: 4, ShippedSessions: 3, UsedSessions: 1},
	}); err != nil {
		t.Fatal(err)
	}

	// Auth: anonymous 401, infer-only 403 (testing.md's admin role gate).
	if code, _, _ := h.do("GET", "/admin/tool-usage", "", ""); code != 401 {
		t.Errorf("anonymous = %d, want 401", code)
	}
	if code, _, _ := h.do("GET", "/admin/tool-usage", "tok-infer", ""); code != 403 {
		t.Errorf("infer-only = %d, want 403", code)
	}

	code, _, body := h.do("GET", "/admin/tool-usage", "tok-admin", "")
	if code != 200 {
		t.Fatalf("admin code=%d body=%s", code, body)
	}
	var payload struct {
		Rows []struct {
			Tool        string `json:"tool"`
			Eligible    bool   `json:"eligible"`
			PrizeTokens int64  `json:"prize_tokens"`
			ShippedReqs int64  `json:"shipped_requests"`
		} `json:"rows"`
		Policy  map[string]any `json:"policy"`
		Summary struct {
			Rows        int   `json:"rows"`
			Clients     int   `json:"clients"`
			PrizeTokens int64 `json:"prize_tokens"`
		} `json:"summary"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(payload.Rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(payload.Rows))
	}
	byTool := map[string]struct {
		Eligible    bool
		PrizeTokens int64
	}{}
	for _, r := range payload.Rows {
		byTool[r.Tool] = struct {
			Eligible    bool
			PrizeTokens int64
		}{r.Eligible, r.PrizeTokens}
	}
	// Ghost: never used, threshold crossed → eligible, prize = 10 × 4.
	if g := byTool["Ghost"]; !g.Eligible || g.PrizeTokens != 40 {
		t.Errorf("Ghost = %+v, want eligible with prize 40", g)
	}
	// Live: used once → sticky ineligible, zero prize.
	if l := byTool["Live"]; l.Eligible || l.PrizeTokens != 0 {
		t.Errorf("Live = %+v, want ineligible with prize 0", l)
	}
	if payload.Summary.PrizeTokens != 40 || payload.Summary.Rows != 2 || payload.Summary.Clients != 1 {
		t.Errorf("summary = %+v, want prize 40 / rows 2 / clients 1", payload.Summary)
	}
	if mode, _ := payload.Policy["mode"].(string); mode != "shadow" {
		t.Errorf("policy.mode = %v, want shadow", payload.Policy["mode"])
	}
	if !strings.Contains(body, `"min_unused_sessions":3`) {
		t.Errorf("policy must echo min_unused_sessions, body: %s", body)
	}
}
