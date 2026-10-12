package server

// Curated protocol-flags admin API. The model id travels in the body (ids can
// contain "/"); a non-release write requires a note; probe writes skip curated
// rows; and a curated openai-only hop becomes a translated tail candidate.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/router"
	"github.com/c4lyp5o/ezllm/internal/store"
)

func protocolPath(acct int64) string { return fmt.Sprintf("/admin/accounts/%d/model-protocol", acct) }

func modelsPath(acct int64) string { return fmt.Sprintf("/admin/accounts/%d/models", acct) }

func listModels(t *testing.T, h *harness, acct int64) []store.ModelEntryRow {
	t.Helper()
	code, _, body := h.do("GET", modelsPath(acct), adminTok(), "")
	if code != 200 {
		t.Fatalf("list models: %d (%s)", code, body)
	}
	var entries []store.ModelEntryRow
	if err := json.Unmarshal([]byte(body), &entries); err != nil {
		t.Fatal(err)
	}
	return entries
}

func entry(t *testing.T, h *harness, acct int64, model string) store.ModelEntryRow {
	t.Helper()
	for _, m := range listModels(t, h, acct) {
		if m.ID == model {
			return m
		}
	}
	t.Fatalf("model %q not in catalog", model)
	return store.ModelEntryRow{}
}

func TestModelProtocolAPIValidation(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	acct := seededAccountID(t, h)
	cases := []struct{ name, body string }{
		{"missing model", `{"proto_openai":true,"note":"x"}`},
		{"nothing to write", `{"model":"m","note":"x"}`},
		{"release mixed with verdicts", `{"model":"m","proto_openai":true,"note":"x","release":true}`},
		{"missing note", `{"model":"m","proto_openai":true}`},
		{"blank note", `{"model":"m","proto_openai":true,"note":"   "}`},
		{"bad verdict type", `{"model":"m","proto_openai":"yes","note":"x"}`},
		{"unknown field rejected", `{"model":"m","proto_openai":true,"note":"x","prto":"x"}`},
		{"malformed json", `{"model":`},
	}
	for _, c := range cases {
		code, _, resp := h.do("PUT", protocolPath(acct), adminTok(), c.body)
		if code != http.StatusBadRequest {
			t.Errorf("%s: got %d want 400 (%s)", c.name, code, resp)
		}
	}
}

func TestModelProtocolUnknownModelIs404(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	acct := seededAccountID(t, h)
	code, _, resp := h.do("PUT", protocolPath(acct), adminTok(), `{"model":"ghost","proto_openai":true,"note":"x"}`)
	if code != http.StatusNotFound {
		t.Fatalf("unknown model: %d (%s), want 404", code, resp)
	}
}

// The round trip: curate → truth echoed + visible in the model list → probe
// writes (single and sweep) leave it alone → release hands the row back and
// probes apply again.
func TestModelProtocolRoundTripAndProbeProtection(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	acct := seededAccountID(t, h)
	if _, err := h.db.Writer().Exec(
		`INSERT INTO models (account_id, model_id, proto_tested_at) VALUES (?, 'm1', '2026-10-12T00:00:00Z')`, acct); err != nil {
		t.Fatal(err)
	}

	code, _, body := h.do("PUT", protocolPath(acct), adminTok(),
		`{"model":"m1","proto_openai":true,"proto_anthropic":false,"note":"edge stonewalls anthropic"}`)
	if code != 200 {
		t.Fatalf("curate: %d (%s), want 200", code, body)
	}
	var echo store.ModelEntryRow
	if err := json.Unmarshal([]byte(body), &echo); err != nil {
		t.Fatal(err)
	}
	if echo.Source != "curated" || echo.Note != "edge stonewalls anthropic" || echo.TestedAt != nil ||
		echo.OpenAI == nil || !*echo.OpenAI || echo.Anthropic == nil || *echo.Anthropic {
		t.Fatalf("echo = %+v", echo)
	}

	// A probe clobber attempt: single write AND the sweep batch.
	yes := true
	if err := h.db.SetModelProto(context.Background(), acct, "m1", provider.SurfaceAnthropic, &yes); err != nil {
		t.Fatal(err)
	}
	if err := h.db.SetModelProtos(context.Background(), acct, map[string]map[string]*bool{
		"m1": {"openai": &yes, "anthropic": &yes},
	}); err != nil {
		t.Fatal(err)
	}
	m := entry(t, h, acct, "m1")
	if m.Source != "curated" || m.Note != "edge stonewalls anthropic" || m.Anthropic == nil || *m.Anthropic {
		t.Fatalf("probe clobbered a curated row: %+v", m)
	}

	// Release: verdicts stay, provenance clears, probes apply again.
	code, _, body = h.do("PUT", protocolPath(acct), adminTok(), `{"model":"m1","release":true}`)
	if code != 200 {
		t.Fatalf("release: %d (%s), want 200", code, body)
	}
	m = entry(t, h, acct, "m1")
	if m.Source != "" || m.Note != "" || m.OpenAI == nil || !*m.OpenAI || m.Anthropic == nil || *m.Anthropic {
		t.Fatalf("after release = %+v", m)
	}
	if err := h.db.SetModelProto(context.Background(), acct, "m1", provider.SurfaceAnthropic, &yes); err != nil {
		t.Fatal(err)
	}
	m = entry(t, h, acct, "m1")
	if m.Anthropic == nil || !*m.Anthropic || m.Source != "probe" {
		t.Fatalf("probe write after release: %+v", m)
	}
}

// A hop curated openai-only becomes a translated tail candidate for an
// anthropic client — asserted through the real admin route, store, resolver
// and dispatcher (plan B test #2).
func TestModelProtocolCuratedFeedsTranslationEligibility(t *testing.T) {
	var hits int
	var wirePath string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		wirePath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"curated-proof"}}]}`))
	}, func(o *Options) { o.Resolver = router.New(o.DB).WithTranslation(true) })
	acct := seededAccountID(t, h)

	// Curated through the ADMIN ROUTE — the API is the gate, not a direct store call.
	code, _, body := h.do("PUT", protocolPath(acct), adminTok(),
		`{"model":"gpt-6-luna","proto_openai":true,"proto_anthropic":false,"note":"curated openai-only"}`)
	if code != 200 {
		t.Fatalf("curate: %d (%s)", code, body)
	}
	if _, err := h.db.UpsertCombo(context.Background(), store.Combo{
		Name: "cur-proof", Strategy: "failover", Enabled: true,
		Hops: []store.ComboHop{{AccountID: acct, ModelID: "gpt-6-luna", Weight: 1}},
	}); err != nil {
		t.Fatal(err)
	}

	code, _, body = h.do("POST", "/v1/messages", "tok-infer",
		`{"model":"cur-proof","max_tokens":8,"messages":[{"role":"user","content":[{"type":"text","text":"ping"}]}]}`)
	if code != 200 || hits != 1 || wirePath != "/chat/completions" {
		t.Fatalf("translated tail: code=%d hits=%d wire=%q (%s)", code, hits, wirePath, body)
	}
	if !strings.Contains(body, `"type":"message"`) || !strings.Contains(body, "curated-proof") {
		t.Fatalf("client body not anthropic-shaped: %s", body)
	}
}
