package proxy

// The token convention that RIDES WITH the translated marker. This is a
// CONSCIOUS decision (2026-10-14, Calypso's call): the synthesized anthropic
// envelope now applies the Messages contract — input_tokens excludes cache and
// the split rides in cache_read_input_tokens — so a translated hop is ledgered
// exactly like a native one (tokens_in = cold tokens, tokens_cached_read = the
// prefix that hit cache, in + cached still the vendor's prompt total). The
// previous behaviour is what this test used to pin: OpenAI counts cached tokens
// inside prompt_tokens and the envelope passed that number straight through, so
// tokens_cached_read stayed 0 on every translated row.

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
	if res.Usage.In != 100 {
		t.Errorf("tokens_in = %d, want 100 (the 1000 prompt total minus the 900 cached)", res.Usage.In)
	}
	if res.Usage.CachedRead != 900 {
		t.Errorf("cached_read = %d, want 900 (the translator now carries the openai cache split)", res.Usage.CachedRead)
	}
	if res.Usage.Out != 42 {
		t.Errorf("tokens_out = %d, want 42", res.Usage.Out)
	}

	// The client sees the same split: in + cached is still the vendor's 1000,
	// so nothing is double-counted while the halves are individually billable
	// (cache reads cost ~10x less than cold input).
	var env struct {
		Usage struct {
			InputTokens       int  `json:"input_tokens"`
			OutputTokens      int  `json:"output_tokens"`
			CacheReadTokens   *int `json:"cache_read_input_tokens"`
			CacheCreateTokens *int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("client body is not JSON: %v", err)
	}
	if env.Usage.InputTokens != 100 || env.Usage.OutputTokens != 42 {
		t.Errorf("client usage = %+v, want input 100 / output 42", env.Usage)
	}
	if env.Usage.CacheReadTokens == nil || *env.Usage.CacheReadTokens != 900 {
		t.Errorf("client cache_read_input_tokens = %v, want 900", env.Usage.CacheReadTokens)
	}
	if env.Usage.CacheCreateTokens != nil {
		t.Errorf("client cache_creation_input_tokens = %v, want absent (upstream reported none)", *env.Usage.CacheCreateTokens)
	}
}
