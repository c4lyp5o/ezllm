package server

// Ledger attribution for translated hops, asserted through the real handler:
// the row is MARKED (surface=anthropic + a hop dialed as openai), and the
// usage recorded for it is the vendor's INCLUSIVE total — a translated hop
// synthesizes the anthropic usage envelope from OpenAI's prompt_tokens, which
// counts the cached prefix, so tokens_in is NOT our cold-token convention and
// tokens_cached_read stays 0. Both facts ride with the marker so the dashboard
// and any cost math can tell the two conventions apart.

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
		`{"model":"gpt-6-luna","proto_openai":true,"proto_anthropic":false,"note":"curated openai-only for ledger attribution"}`); code != 200 {
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
	// INCLUSIVE for translated rows: the openai total, cache NOT subtracted.
	if row.TokensIn != 1000 {
		t.Errorf("tokens_in = %d, want 1000 (inclusive openai total)", row.TokensIn)
	}
	if row.TokensCachedRead != 0 {
		t.Errorf("tokens_cached_read = %d, want 0 (translator does not carry the openai cache split)", row.TokensCachedRead)
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
