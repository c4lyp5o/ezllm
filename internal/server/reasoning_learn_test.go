package server

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

const sparkReasoningRefusal = `{"model":"muse-spark","error":{"message":"reasoning_effort 'none' is not supported for model 'muse-spark'. Supported values: [minimal, low, medium, high, xhigh, max]","param":"reasoning.effort"}}`

// The last link: what the proxy learned must reach the store through the
// handler that owns it. Without this the proxy reports the fact and it dies
// with the request.
func TestRefusalTeachesTheStoreThroughTheHandler(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, sparkReasoningRefusal)
	})

	code, _, body := h.do("POST", "/v1/chat/completions", "tok-infer",
		`{"model":"up/gpt-6-luna","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"none"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("the refusal must reach the client unchanged, got %d %s", code, body)
	}

	var vocab, required any
	var model string
	err := h.db.Reader().QueryRow(
		`SELECT model_id, reasoning_vocab, reasoning_required FROM models WHERE reasoning_vocab IS NOT NULL`,
	).Scan(&model, &vocab, &required)
	if err != nil {
		t.Fatalf("no row learned a reasoning vocabulary: %v", err)
	}
	if !strings.Contains(vocab.(string), "minimal") {
		t.Fatalf("%s: vocabulary = %v, want the provider's own list", model, vocab)
	}
}
