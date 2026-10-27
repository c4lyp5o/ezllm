package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// shrinkingAdapter rewrites the outbound body to something SHORTER, the way
// the Copilot adapter does when it drops system messages, and sets the
// matching Content-Length itself.
type shrinkingAdapter struct {
	provider.Adapter
	body string
}

func (a shrinkingAdapter) PrepareRequest(ctx context.Context, req *http.Request, acct provider.Account, key string, surface provider.Surface) error {
	if err := a.Adapter.PrepareRequest(ctx, req, acct, key, surface); err != nil {
		return err
	}
	req.Body = io.NopCloser(strings.NewReader(a.body))
	req.ContentLength = int64(len(a.body))
	return nil
}

// An adapter that rewrites the body owns its Content-Length: attempt must not
// stamp the pre-adapter length back on, or the client refuses the mismatch
// ("http: ContentLength=N with Body length M") and the hop is charged a
// transport fault it never had.
func TestAttemptKeepsAdapterRewrittenContentLength(t *testing.T) {
	const patched = `{"model":"resolved-model","messages":[{"role":"user","content":"hi"}]}`
	var got string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"message":{"content":"OK"}}]}`))
	}))
	defer upstream.Close()

	client := &http.Client{Timeout: 30 * time.Second}
	reg := provider.NewRegistry(client)
	base, err := reg.Get(provider.KindOpenAICompatible)
	if err != nil {
		t.Fatal(err)
	}
	reg.Register(shrinkingAdapter{Adapter: base, body: patched})
	d := NewDispatcher(client, reg)

	reqBody, _, _ := SwapModel([]byte(`{"model":"whatever","messages":[{"role":"system","content":"a long persona the adapter drops"},{"role":"user","content":"hi"}]}`), "resolved-model")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(reqBody)))
	res, err := d.Forward(context.Background(), rec, req, testRoute(upstream.URL, provider.SurfaceChat), reqBody)
	if err != nil {
		t.Fatalf("forward failed: %v", err)
	}
	if res.Status != http.StatusOK {
		t.Errorf("status = %d, want 200", res.Status)
	}
	if got != patched {
		t.Errorf("upstream saw %q, want the adapter's body %q", got, patched)
	}
}
