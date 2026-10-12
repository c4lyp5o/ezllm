package proxy

// The token convention that RIDES WITH the translated marker: a translated row's
// tokens_in is the vendor's INCLUSIVE total. OpenAI counts cached tokens inside
// prompt_tokens, and the synthesized anthropic envelope passes that number
// straight through as input_tokens, so the tap reads it as-is and
// tokens_cached_read stays 0. Pinned here so a later "normalize it like the
// native path" change has to be a conscious decision that also updates the
// ledger marker's documented meaning.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTranslatedUsageStaysInclusive(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c9","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":1000,"completion_tokens":42,"prompt_tokens_details":{"cached_tokens":900,"cached_tokens_audio":0}}}`))
	}))
	defer upstream.Close()

	clientBody := `{"model":"daily","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	d := newDispatcher()
	d.SetRetries(0)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(clientBody))
	rec := httptest.NewRecorder()
	_, res, err := d.ForwardCandidates(context.Background(), rec, req,
		[]Route{translatedRoute(upstream.URL, "deepseek-flash")}, []byte(clientBody))
	if err != nil {
		t.Fatal(err)
	}
	if res.Usage == nil {
		t.Fatal("no usage tapped from the translated body")
	}
	if res.Usage.In != 1000 {
		t.Errorf("tokens_in = %d, want 1000 (the INCLUSIVE openai total, not prompt-cached)", res.Usage.In)
	}
	if res.Usage.CachedRead != 0 {
		t.Errorf("cached_read = %d, want 0 — the translator carries no openai cache split", res.Usage.CachedRead)
	}
	if res.Usage.Out != 42 {
		t.Errorf("tokens_out = %d, want 42", res.Usage.Out)
	}

	// The client sees the same inclusive number: the synthesized envelope is
	// what an anthropic client would cache-bill against.
	var env struct {
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("client body is not JSON: %v", err)
	}
	if env.Usage.InputTokens != 1000 || env.Usage.OutputTokens != 42 {
		t.Errorf("client usage = %+v, want input 1000 / output 42", env.Usage)
	}
}
