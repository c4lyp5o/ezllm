package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/store"
)

func postProfile(t *testing.T, h *harness, token, body string) (int, http.Header, string) {
	t.Helper()
	return h.do("POST", "/admin/compression-profiles", token, body)
}

func itoa(id int64) string { return strconv.FormatInt(id, 10) }

// doWithExtra is do() plus extra request headers (x-ezllm-compression etc).
// In-process like do(): the harness never assigns base — httptest.NewRequest
// against the handler directly.
func (h *harness) doWithExtra(method, path, token, body string, extra map[string]string) (int, http.Header, string) {
	h.t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec.Code, rec.Header(), rec.Body.String()
}

// CRUD: create → list → patch → get → delete, plus validation rejections.
func TestCompressionProfileCRUD(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})

	code, _, body := postProfile(t, h, "tok-admin", `{
		"name": "agentic-safe",
		"stages": [{"engine":"session_dedup"},{"engine":"rtk","options":{"keep_lines":4}}]
	}`)
	if code != http.StatusCreated {
		t.Fatalf("create = %d: %s", code, body)
	}
	var created store.CompressionProfile
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == 0 || !created.Enabled || !created.ExemptLastTurn || !created.FailOpen {
		t.Errorf("schema defaults missing: %+v", created)
	}
	if created.MinCompressRatio != 0.05 {
		t.Errorf("min_compress_ratio default = %v, want 0.05", created.MinCompressRatio)
	}

	// unknown engine rejected at the door (not silently fail-open forever)
	code, _, body = postProfile(t, h, "tok-admin",
		`{"name":"bad","stages":[{"engine":"caveman"}]}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "unknown engine") {
		t.Errorf("unknown engine = %d %s, want 400 mentioning it", code, body)
	}

	// empty pipeline rejected
	code, _, _ = postProfile(t, h, "tok-admin", `{"name":"empty","stages":[]}`)
	if code != http.StatusBadRequest {
		t.Errorf("empty stages = %d, want 400", code)
	}

	// list carries the full pipeline (combo editor reads this row)
	code, _, body = h.do("GET", "/admin/compression-profiles", "tok-admin", "")
	if code != 200 || !strings.Contains(body, "agentic-safe") || !strings.Contains(body, "session_dedup") {
		t.Fatalf("list = %d %s", code, body)
	}

	// patch a gate, name and stages preserved
	code, _, body = h.do("PATCH", "/admin/compression-profiles/"+itoa(created.ID),
		"tok-admin", `{"min_compress_ratio":0.15}`)
	if code != 200 {
		t.Fatalf("patch = %d: %s", code, body)
	}
	code, _, body = h.do("GET", "/admin/compression-profiles/"+itoa(created.ID), "tok-admin", "")
	if code != 200 || !strings.Contains(body, "0.15") || !strings.Contains(body, "agentic-safe") {
		t.Fatalf("patch not persisted: %d %s", code, body)
	}

	// out-of-range gate rejected
	code, _, _ = h.do("PATCH", "/admin/compression-profiles/"+itoa(created.ID),
		"tok-admin", `{"min_compress_ratio":1.5}`)
	if code != http.StatusBadRequest {
		t.Errorf("ratio 1.5 = %d, want 400", code)
	}

	// admin-only (infer token rejected)
	code, _, _ = h.do("POST", "/admin/compression-profiles", "tok-infer",
		`{"name":"nope","stages":[{"engine":"rtk"}]}`)
	if code != http.StatusForbidden {
		t.Errorf("infer token create = %d, want 403", code)
	}

	code, _, _ = h.do("DELETE", "/admin/compression-profiles/"+itoa(created.ID), "tok-admin", "")
	if code != 200 {
		t.Fatalf("delete = %d", code)
	}
	code, _, _ = h.do("GET", "/admin/compression-profiles/"+itoa(created.ID), "tok-admin", "")
	if code != http.StatusNotFound {
		t.Errorf("get after delete = %d, want 404", code)
	}
}

// A combo pointing at a profile must block deletion (silent detach would
// change routing behavior nobody asked to change).
func TestCompressionProfileDeleteRefusedWhenReferenced(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()

	pid, err := h.db.UpsertCompressionProfile(ctx, store.CompressionProfile{
		Name: "p1", Enabled: true, Stages: json.RawMessage(`[{"engine":"rtk"}]`),
		ExemptLastTurn: true, MinCompressRatio: 0.05, FailOpen: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var acctID int64
	if err := h.db.Reader().QueryRowContext(ctx, `SELECT id FROM accounts LIMIT 1`).Scan(&acctID); err != nil {
		t.Fatal(err)
	}
	var modelID string
	if err := h.db.Reader().QueryRowContext(ctx, `SELECT model_id FROM models LIMIT 1`).Scan(&modelID); err != nil {
		t.Fatal(err)
	}
	cpid := pid
	if _, err := h.db.UpsertCombo(ctx, store.Combo{
		Name: "comped", Strategy: "failover", Enabled: true, CompressionProfileID: &cpid,
		Hops: []store.ComboHop{{AccountID: acctID, ModelID: modelID, Weight: 1}},
	}); err != nil {
		t.Fatal(err)
	}

	code, _, body := h.do("DELETE", "/admin/compression-profiles/"+itoa(pid), "tok-admin", "")
	if code != 422 {
		t.Fatalf("delete referenced profile = %d (%s), want 422", code, body)
	}
}

// The money test: a real inference call with the override header — the
// upstream must RECEIVE the compressed body, and the ledger must tell the
// truth (contract rules 2 and 6).
func TestCompressionAppliesAtInferenceAndLedgers(t *testing.T) {
	var gotBody []byte
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","choices":[{"message":{"role":"assistant","content":"ok"}}],
			"usage":{"prompt_tokens":10,"completion_tokens":2}}`))
	})
	ctx := context.Background()

	if _, err := h.db.UpsertCompressionProfile(ctx, store.CompressionProfile{
		Name: "agentic-safe", Enabled: true,
		Stages:           json.RawMessage(`[{"engine":"session_dedup"},{"engine":"rtk","options":{"keep_lines":3}}]`),
		ExemptLastTurn:   true,
		MinCompressRatio: 0.05,
		FailOpen:         true,
	}); err != nil {
		t.Fatal(err)
	}

	// history with a fat duplicate + a noisy tool output
	dupSystem := strings.Repeat("You are a terse assistant. Follow the style guide exactly. ", 20)
	noisy := "$ make test\n"
	for i := 0; i < 60; i++ {
		noisy += "  compiled pkg/file" + string(rune('a'+i%26)) + ".o\n"
	}
	noisy += "FAIL pkg/router (error: nil deref)\n"
	for i := 0; i < 60; i++ {
		noisy += "  compiled pkg/x" + string(rune('a'+i%26)) + ".o\n"
	}
	origMsgs := []any{
		map[string]any{"role": "system", "content": dupSystem},
		map[string]any{"role": "user", "content": "run the suite and fix what breaks"},
		map[string]any{"role": "assistant", "content": "running tests"},
		map[string]any{"role": "tool", "content": noisy},
		map[string]any{"role": "system", "content": dupSystem},      // duplicate: must drop
		map[string]any{"role": "user", "content": "final question"}, // final: must survive
	}
	origBody, _ := json.Marshal(map[string]any{
		// direct routes are namespace/model — bare ids only ever match combos
		"model": "up/gpt-6-luna", "stream": false, "messages": origMsgs,
		"custom_field": map[string]any{"x": 1},
	})

	code, _, resp := h.doWithExtra("POST", "/v1/chat/completions", "tok-infer", string(origBody),
		map[string]string{"x-ezllm-compression": "agentic-safe"})
	if code != 200 {
		t.Fatalf("inference = %d: %s", code, resp)
	}

	// ── the upstream must have received the COMPRESSED body ──
	var up map[string]any
	if err := json.Unmarshal(gotBody, &up); err != nil {
		t.Fatalf("upstream body not JSON: %v", err)
	}
	if up["custom_field"] == nil || up["model"] != "gpt-6-luna" || up["stream"] != false {
		t.Errorf("non-message fields lost in transit: %v", up)
	}
	upMsgs := up["messages"].([]any)
	if len(upMsgs) != 5 {
		t.Errorf("duplicate system not dropped: %d messages", len(upMsgs))
	}
	// final message byte-identical (contract rule 1)
	finalOrig, _ := json.Marshal(origMsgs[len(origMsgs)-1])
	finalUp, _ := json.Marshal(upMsgs[len(upMsgs)-1])
	if string(finalOrig) != string(finalUp) {
		t.Errorf("final message changed:\n orig=%s\n  up=%s", finalOrig, finalUp)
	}
	// tool output filtered, error line kept (contract rule 2 via rtk scope)
	toolUp := upMsgs[3].(map[string]any)["content"].(string)
	if !strings.Contains(toolUp, "FAIL pkg/router") {
		t.Errorf("rtk lost the error line: %q", toolUp)
	}
	if len(toolUp) >= len(noisy) {
		t.Errorf("rtk did not shrink tool output: %d -> %d", len(noisy), len(toolUp))
	}

	// ── the ledger must tell the truth (contract rule 6) ──
	if err := h.db.Flush(); err != nil {
		t.Fatal(err)
	}
	var profile string
	var applied int
	var pre, saved int64
	var cms *int64
	err := h.db.Reader().QueryRowContext(ctx, `
SELECT compression_profile, compression_applied, prompt_tokens_pre, tokens_saved, compression_ms
FROM calls ORDER BY ts DESC LIMIT 1`).Scan(&profile, &applied, &pre, &saved, &cms)
	if err != nil {
		t.Fatalf("ledger read: %v", err)
	}
	if profile != "agentic-safe" || applied != 1 {
		t.Errorf("ledger profile/applied = %q/%d, want agentic-safe/1", profile, applied)
	}
	if pre == 0 || saved == 0 {
		t.Errorf("ledger pre/saved = %d/%d, both must be >0", pre, saved)
	}
	if cms == nil {
		t.Error("compression_ms is NULL, want recorded")
	}

	// ── x-ezllm-compression: off must ship the ORIGINAL, with no profile
	// resolved (contract: no profile → columns stay zero) ──
	gotBody = nil
	code, _, resp = h.doWithExtra("POST", "/v1/chat/completions", "tok-infer", string(origBody),
		map[string]string{"x-ezllm-compression": "off"})
	if code != 200 {
		t.Fatalf("off inference = %d: %s", code, resp)
	}
	var upOff map[string]any
	_ = json.Unmarshal(gotBody, &upOff)
	offMsgs := upOff["messages"].([]any)
	if len(offMsgs) != len(origMsgs) {
		t.Errorf("off sent %d msgs, want original %d", len(offMsgs), len(origMsgs))
	}
	if err := h.db.Flush(); err != nil {
		t.Fatal(err)
	}
	var profile2 string
	var applied2 int
	var pre2 int64
	if err := h.db.Reader().QueryRowContext(ctx, `
SELECT compression_profile, compression_applied, prompt_tokens_pre
FROM calls ORDER BY ts DESC LIMIT 1`).Scan(&profile2, &applied2, &pre2); err != nil {
		t.Fatal(err)
	}
	// the explicit ask is RECORDED ("off") with nothing applied — a request
	// that wanted compression and got none is visible in the feed, while a
	// request that never mentioned compression stays blank.
	if profile2 != "off" || applied2 != 0 || pre2 != 0 {
		t.Errorf("off row = %q/%d/%d, want off/0/0", profile2, applied2, pre2)
	}
}

// The combo path: no header, profile rides the combo's compression_profile_id.
func TestCompressionResolvesFromCombo(t *testing.T) {
	var gotBody []byte
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","choices":[{"message":{"role":"assistant","content":"ok"}}],
			"usage":{"prompt_tokens":10,"completion_tokens":2}}`))
	})
	ctx := context.Background()

	pid, err := h.db.UpsertCompressionProfile(ctx, store.CompressionProfile{
		Name: "combo-profile", Enabled: true,
		Stages: json.RawMessage(`[{"engine":"session_dedup"}]`), ExemptLastTurn: true,
		MinCompressRatio: 0.05, FailOpen: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var acctID int64
	if err := h.db.Reader().QueryRowContext(ctx, `SELECT id FROM accounts LIMIT 1`).Scan(&acctID); err != nil {
		t.Fatal(err)
	}
	var modelID string
	if err := h.db.Reader().QueryRowContext(ctx, `SELECT model_id FROM models LIMIT 1`).Scan(&modelID); err != nil {
		t.Fatal(err)
	}
	cpid := pid
	if _, err := h.db.UpsertCombo(ctx, store.Combo{
		Name: "compressed", Strategy: "failover", Enabled: true, CompressionProfileID: &cpid,
		Hops: []store.ComboHop{{AccountID: acctID, ModelID: modelID, Weight: 1}},
	}); err != nil {
		t.Fatal(err)
	}

	dup := strings.Repeat("Persistent context line. ", 30)
	body, _ := json.Marshal(map[string]any{
		"model": "compressed", // combo name, bare shape
		"messages": []any{
			map[string]any{"role": "system", "content": dup},
			map[string]any{"role": "user", "content": "go"},
			map[string]any{"role": "system", "content": dup}, // duplicate
			map[string]any{"role": "user", "content": "final"},
		},
	})
	code, _, resp := h.do("POST", "/v1/chat/completions", "tok-infer", string(body))
	if code != 200 {
		t.Fatalf("combo inference = %d: %s", code, resp)
	}
	var up map[string]any
	_ = json.Unmarshal(gotBody, &up)
	if len(up["messages"].([]any)) != 3 {
		t.Errorf("combo path did not compress: %d msgs", len(up["messages"].([]any)))
	}
	if up["model"] != modelID {
		t.Errorf("combo model swap broken: %v", up["model"])
	}
	if err := h.db.Flush(); err != nil {
		t.Fatal(err)
	}
	var profile string
	var applied int
	if err := h.db.Reader().QueryRowContext(ctx, `
SELECT compression_profile, compression_applied FROM calls ORDER BY ts DESC LIMIT 1`).
		Scan(&profile, &applied); err != nil {
		t.Fatal(err)
	}
	if profile != "combo-profile" || applied != 1 {
		t.Errorf("combo ledger = %q/%d, want combo-profile/1", profile, applied)
	}
}
