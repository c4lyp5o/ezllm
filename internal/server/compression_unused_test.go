package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/store"
)

// With no compression profiles, a request skips the key and combo lookups and
// reaches the provider exactly as the app sent it. To see the skip, the test
// warms the "no profiles" answer, then writes a profile and attaches it to the
// key behind the store's back: the request must still go out untouched. Once a
// profile is saved the normal way, the key's profile applies again.
func TestNoProfilesSkipsCompressionLookups(t *testing.T) {
	var gotBody []byte
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`))
	})
	ctx := context.Background()
	if h.db.HasCompressionProfiles(ctx) {
		t.Fatal("precondition: no profiles yet")
	}
	res, err := h.db.Writer().Exec(`INSERT INTO compression_profiles (name, enabled, stages, exempt_last_turn, min_compress_ratio, fail_open, auto_trigger_tokens, fresh_tool_output, notes)
		VALUES ('hidden', 1, '[{"engine":"session_dedup"}]', 1, 0.05, 1, 0, 'keep', '')`)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := res.LastInsertId()
	if _, err := h.db.Writer().Exec(`UPDATE client_tokens SET compression_profile_id = ? WHERE name = 'my-app'`, pid); err != nil {
		t.Fatal(err)
	}

	var ns, modelID string
	if err := h.db.Reader().QueryRowContext(ctx, `SELECT a.namespace, m.model_id FROM models m JOIN accounts a ON a.id=m.account_id LIMIT 1`).Scan(&ns, &modelID); err != nil {
		t.Fatal(err)
	}
	dup := strings.Repeat("Persistent context line. ", 30)
	body, _ := json.Marshal(map[string]any{"model": ns + "/" + modelID, "messages": []any{
		map[string]any{"role": "system", "content": dup}, map[string]any{"role": "user", "content": "go"},
		map[string]any{"role": "system", "content": dup}, map[string]any{"role": "user", "content": "final"},
	}})
	send := func() int {
		if code, _, resp := h.do("POST", "/v1/chat/completions", "tok-infer", string(body)); code != 200 {
			t.Fatalf("inference: %d %s", code, resp)
		}
		var up map[string]any
		_ = json.Unmarshal(gotBody, &up)
		return len(up["messages"].([]any))
	}
	if n := send(); n != 4 {
		t.Fatalf("with no profiles known, the request sent %d messages, want all 4 untouched", n)
	}
	if _, err := h.db.UpsertCompressionProfile(ctx, store.CompressionProfile{Name: "another", Enabled: true, Stages: json.RawMessage(`[]`)}); err != nil {
		t.Fatal(err)
	}
	if n := send(); n != 3 {
		t.Fatalf("after a profile is saved, the key's profile should apply: sent %d messages, want 3", n)
	}
}
