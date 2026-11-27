package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/store"
)

// A key's own compression profile applies to its direct namespace/model
// requests; null clears it; an unknown profile is refused.
func TestKeyCompressionProfileOnDirectRoute(t *testing.T) {
	var gotBody []byte
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`))
	})
	ctx := context.Background()
	pid, err := h.db.UpsertCompressionProfile(ctx, store.CompressionProfile{
		Name: "key-profile", Enabled: true, Stages: json.RawMessage(`[{"engine":"session_dedup"}]`),
		ExemptLastTurn: true, MinCompressRatio: 0.05, FailOpen: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var keyID int64
	if err := h.db.Reader().QueryRowContext(ctx, `SELECT id FROM client_tokens WHERE name='my-app'`).Scan(&keyID); err != nil {
		t.Fatal(err)
	}
	if code, resp := jsonDo(t, h, "PATCH", fmt.Sprintf("/admin/tokens/%d", keyID), "tok-admin", map[string]any{"compression_profile_id": 999}); code != http.StatusBadRequest {
		t.Fatalf("unknown profile: %d %s", code, resp)
	}
	code, resp := jsonDo(t, h, "PATCH", fmt.Sprintf("/admin/tokens/%d", keyID), "tok-admin", map[string]any{"compression_profile_id": pid})
	if code != http.StatusOK || mustJSON(t, resp)["compression_profile_id"] != float64(pid) {
		t.Fatalf("set profile: %d %s", code, resp)
	}

	var ns, modelID string
	if err := h.db.Reader().QueryRowContext(ctx, `SELECT a.namespace, m.model_id FROM models m JOIN accounts a ON a.id=m.account_id LIMIT 1`).Scan(&ns, &modelID); err != nil {
		t.Fatal(err)
	}
	dup := strings.Repeat("Persistent context line. ", 30)
	send := func() int {
		body, _ := json.Marshal(map[string]any{"model": ns + "/" + modelID, "messages": []any{
			map[string]any{"role": "system", "content": dup}, map[string]any{"role": "user", "content": "go"},
			map[string]any{"role": "system", "content": dup}, map[string]any{"role": "user", "content": "final"},
		}})
		if code, _, resp := h.do("POST", "/v1/chat/completions", "tok-infer", string(body)); code != 200 {
			t.Fatalf("inference: %d %s", code, resp)
		}
		var up map[string]any
		_ = json.Unmarshal(gotBody, &up)
		return len(up["messages"].([]any))
	}
	if n := send(); n != 3 {
		t.Fatalf("direct route with a key profile sent %d messages, want 3 (duplicate system dropped)", n)
	}
	if code, resp := jsonDo(t, h, "PATCH", fmt.Sprintf("/admin/tokens/%d", keyID), "tok-admin", map[string]any{"compression_profile_id": nil}); code != http.StatusOK {
		t.Fatalf("clear: %d %s", code, resp)
	}
	if n := send(); n != 4 {
		t.Fatalf("after clearing, the direct route sent %d messages, want 4 (uncompressed)", n)
	}
}
