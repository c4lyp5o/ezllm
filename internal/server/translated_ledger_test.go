package server

// Ledger attribution for translated hops, asserted through the real handler:
// the row is MARKED (surface=anthropic + a hop dialed as openai), and its usage
// now follows the SAME convention as a native row. The synthesized Messages
// envelope subtracts the cache from input_tokens and carries the split
// (cache_read_input_tokens), so the tap records tokens_in = cold tokens and
// tokens_cached_read = the prefix that hit cache — before this projection
// carried no split, translated rows ledgered the vendor's inclusive total with
// cached=0 and were the one documented exception to the invariant.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/store"
)

func TestTranslatedCallIsAttributedInTheLedger(t *testing.T) {
	upstream := `{"id":"c1","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":1000,"completion_tokens":42,"prompt_tokens_details":{"cached_tokens":900}}}`
	var hits int
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(upstream))
	}, func(o *Options) { o.Resolver = o.Resolver.WithTranslation(true) })
	acct := seededAccountID(t, h)

	// Curate through the ADMIN ROUTE — the API is the gate.
	if code, _, body := h.do("PUT", protocolPath(acct), adminTok(),
		`{"model":"gpt-6-luna","proto_chat":true,"proto_anthropic":false,"note":"curated openai-only for ledger attribution"}`); code != 200 {
		t.Fatalf("curate: %d (%s)", code, body)
	}
	if _, err := h.db.UpsertCombo(context.Background(), store.Combo{
		Name: "attrib", Strategy: "failover", Enabled: true,
		Hops: []store.ComboHop{{AccountID: acct, ModelID: "gpt-6-luna", Weight: 1}},
	}); err != nil {
		t.Fatal(err)
	}

	code, _, body := h.do("POST", "/v1/messages", "tok-infer",
		`{"model":"attrib","max_tokens":8,"messages":[{"role":"user","content":[{"type":"text","text":"ping"}]}]}`)
	if code != 200 || hits != 1 {
		t.Fatalf("call: code=%d hits=%d (%s)", code, hits, body)
	}
	// The ledger is async + batched; flush so the row is visible.
	if err := h.db.Flush(); err != nil {
		t.Fatal(err)
	}

	rows, err := h.db.RecentCalls(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("ledger rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if !row.Translated {
		t.Fatal("translated hop was not marked in the ledger")
	}
	// Same convention as a native row: prompt_tokens 1000 INCLUDES the 900
	// cached, so the anthropic envelope reports input=100 + cache_read=900 and
	// the tap lands exactly that. in + cached still equals the vendor's 1000.
	if row.TokensIn != 100 {
		t.Errorf("tokens_in = %d, want 100 (inclusive 1000 minus the 900 cached)", row.TokensIn)
	}
	if row.TokensCachedRead != 900 {
		t.Errorf("tokens_cached_read = %d, want 900 (the translator now carries the openai cache split)", row.TokensCachedRead)
	}
	if row.Surface != "anthropic" {
		t.Errorf("surface = %q, want the CLIENT surface", row.Surface)
	}

	// The dashboard projection must expose the same marker, so translated
	// traffic is countable from the API without reading the store.
	code, _, body = h.do("GET", "/admin/requests?limit=10", adminTok(), "")
	if code != 200 {
		t.Fatalf("requests: %d (%s)", code, body)
	}
	var out struct {
		Rows []map[string]any `json:"rows"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Rows) != 1 {
		t.Fatalf("projected rows = %d, want 1", len(out.Rows))
	}
	if got, ok := out.Rows[0]["translated"].(bool); !ok || !got {
		t.Errorf("projection translated = %v, want true", out.Rows[0]["translated"])
	}
}

// An identical request served by a NATIVE anthropic hop must never be marked.
func TestNativeHopIsNotMarkedTranslated(t *testing.T) {
	var hits int
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"m1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],
			"model":"m1","stop_reason":"end_turn","stop_sequence":null,
			"usage":{"input_tokens":7,"output_tokens":2}}`))
	}, func(o *Options) { o.Resolver = o.Resolver.WithTranslation(true) })

	// A second account speaking the anthropic wire natively, with its own model.
	up := provider.Account{Name: "nat", Namespace: "nat", Kind: provider.KindAnthropicCompat,
		BaseURL: h.upstream.URL, Enabled: true}
	acct, err := h.db.UpsertAccount(context.Background(), up)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.db.AddKey(context.Background(), acct, "primary", "«redacted:sk-…»"); err != nil {
		t.Fatal(err)
	}
	if err := h.db.UpsertModels(context.Background(), acct, []provider.ModelInfo{{ID: "m1", OwnedBy: "nat"}}); err != nil {
		t.Fatal(err)
	}
	yes := true
	if err := h.db.SetModelProto(context.Background(), acct, "m1", provider.SurfaceAnthropic, &yes); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.UpsertCombo(context.Background(), store.Combo{
		Name: "native-proof", Strategy: "failover", Enabled: true,
		Hops: []store.ComboHop{{AccountID: acct, ModelID: "m1", Weight: 1}},
	}); err != nil {
		t.Fatal(err)
	}

	code, _, body := h.do("POST", "/v1/messages", "tok-infer",
		`{"model":"native-proof","max_tokens":8,"messages":[{"role":"user","content":[{"type":"text","text":"ping"}]}]}`)
	if code != 200 || hits != 1 {
		t.Fatalf("native call: code=%d hits=%d (%s)", code, hits, body)
	}
	if err := h.db.Flush(); err != nil {
		t.Fatal(err)
	}
	rows, err := h.db.RecentCalls(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].Translated {
		t.Error("a native anthropic hop must never be marked translated")
	}
}

// A hop curated onto the RESPONSES wire is also translated traffic — same
// marker, no second vocabulary. A chat client asking such a hop must land a
// ledger row with translated=true and the CLIENT's surface, with the responses
// usage total passed through as tokens_in.
func TestResponsesWireHopIsMarkedTranslated(t *testing.T) {
	var hits int
	var wirePath string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		wirePath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"resp_1","object":"response","status":"completed",
			"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],
			"usage":{"input_tokens":500,"output_tokens":7}}`))
	}, func(o *Options) {
		o.Resolver = o.Resolver.WithResponsesModels([]string{"up/gpt-6-luna"})
	})
	acct := seededAccountID(t, h)
	if _, err := h.db.UpsertCombo(context.Background(), store.Combo{
		Name: "resp-proof", Strategy: "failover", Enabled: true,
		Hops: []store.ComboHop{{AccountID: acct, ModelID: "gpt-6-luna", Weight: 1}},
	}); err != nil {
		t.Fatal(err)
	}

	code, _, body := h.do("POST", "/v1/chat/completions", "tok-infer",
		`{"model":"resp-proof","max_tokens":8,"messages":[{"role":"user","content":"ping"}]}`)
	if code != 200 || hits != 1 {
		t.Fatalf("call: code=%d hits=%d (%s)", code, hits, body)
	}
	if wirePath != "/responses" {
		t.Fatalf("hop must be dialed on the responses wire, got %q", wirePath)
	}
	if err := h.db.Flush(); err != nil {
		t.Fatal(err)
	}
	rows, err := h.db.RecentCalls(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if !row.Translated {
		t.Error("a responses-wire hop must carry the translated marker (one marker, one meaning)")
	}
	if row.Surface != "chat" {
		t.Errorf("surface = %q, want the CLIENT surface (openai)", row.Surface)
	}
	if row.TokensIn != 500 {
		t.Errorf("tokens_in = %d, want 500 (the responses total passed through)", row.TokensIn)
	}
}
