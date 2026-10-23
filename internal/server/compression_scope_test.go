package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/store"
)

// An explicit X-Ezllm-Session is caller-chosen, so two clients can send the
// same value. The memo behind it must still be per client: client B's request
// may not replay forms client A committed. Here A primes call two as a
// tool_ref reference to call one; B sends a different call one, and call two
// must reach the upstream as B's own original output.
func TestExplicitSessionMemoIsPerClient(t *testing.T) {
	var captured []byte
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	})
	ctx := context.Background()
	if err := h.db.UpsertClientToken(ctx, "other", "tok-other", "infer"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.UpsertCompressionProfile(ctx, store.CompressionProfile{Name: "scope-ref", Enabled: true, ExemptLastTurn: true, FailOpen: true,
		Stages: json.RawMessage(`[{"engine":"tool_ref","options":{"min_bytes":100}}]`)}); err != nil {
		t.Fatal(err)
	}
	x := strings.Repeat("ORIGINAL_OUTPUT ", 60)
	y := strings.Repeat("DIFFERENT_OUTPUT ", 60)
	body := func(first string) string {
		call := func(id string) map[string]any {
			return map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": id, "type": "function",
				"function": map[string]any{"name": "read_file", "arguments": "{}"}}}}
		}
		b, _ := json.Marshal(map[string]any{"model": "up/gpt-6-luna", "messages": []any{
			call("one"), map[string]any{"role": "tool", "tool_call_id": "one", "content": first},
			call("two"), map[string]any{"role": "tool", "tool_call_id": "two", "content": x},
			map[string]any{"role": "user", "content": "continue"}}})
		return string(b)
	}
	callTwo := func() string {
		var root struct {
			Messages []map[string]any `json:"messages"`
		}
		if err := json.Unmarshal(captured, &root); err != nil || len(root.Messages) < 4 {
			t.Fatalf("upstream body %s: %v", captured, err)
		}
		s, _ := root.Messages[3]["content"].(string)
		return s
	}
	extra := map[string]string{"X-Ezllm-Session": "shared-public-session", "X-Ezllm-Compression": "scope-ref"}
	if code, _, resp := h.doWithExtra("POST", "/v1/chat/completions", "tok-infer", body(x), extra); code != 200 {
		t.Fatalf("prime %d %s", code, resp)
	}
	if got := callTwo(); got == x {
		t.Fatalf("precondition: client A's call two should ship as a reference, got the original")
	}
	if code, _, resp := h.doWithExtra("POST", "/v1/chat/completions", "tok-other", body(y), extra); code != 200 {
		t.Fatalf("client B %d %s", code, resp)
	}
	if got := callTwo(); got != x {
		t.Fatalf("client B's call two was replaced by client A's committed form: %q", got)
	}
}

// The session's profile binding is per client too: client B naming the same
// session under its own profile binds that profile, not the version client A
// bound first.
func TestExplicitSessionBindingIsPerClient(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	})
	ctx := context.Background()
	if err := h.db.UpsertClientToken(ctx, "other", "tok-other", "infer"); err != nil {
		t.Fatal(err)
	}
	for name, minBytes := range map[string]int{"bind-a": 100, "bind-b": 150} {
		if _, err := h.db.UpsertCompressionProfile(ctx, store.CompressionProfile{Name: name, Enabled: true, ExemptLastTurn: true, FailOpen: true,
			Stages: json.RawMessage(`[{"engine":"tool_ref","options":{"min_bytes":` + strconv.Itoa(minBytes) + `}}]`)}); err != nil {
			t.Fatal(err)
		}
	}
	body := `{"model":"up/gpt-6-luna","messages":[{"role":"user","content":"first"},{"role":"assistant","content":"ok"},{"role":"user","content":"next"}]}`
	for _, c := range [][2]string{{"tok-infer", "bind-a"}, {"tok-other", "bind-b"}} { // A binds first
		if code, _, resp := h.doWithExtra("POST", "/v1/chat/completions", c[0], body,
			map[string]string{"X-Ezllm-Session": "shared-binding-session", "X-Ezllm-Compression": c[1]}); code != 200 {
			t.Fatalf("%s %d %s", c[0], code, resp)
		}
	}
	if err := h.db.Flush(); err != nil {
		t.Fatal(err)
	}
	versions := map[string]string{}
	rows, err := h.db.Reader().Query(`SELECT compression_profile, compression_version FROM calls`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var p, v string
		if err := rows.Scan(&p, &v); err != nil {
			t.Fatal(err)
		}
		versions[p] = v
	}
	if versions["bind-a"] == "" || versions["bind-b"] == "" || versions["bind-a"] == versions["bind-b"] {
		t.Fatalf("client B's row carries client A's bound version: %v", versions)
	}
}
