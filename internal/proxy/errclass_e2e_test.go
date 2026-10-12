package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// The ledger err column carries the semantic cause in brackets ahead of the
// upstream's own words — a real opencode-style body classifies as
// upstream_capacity, a tokenrouter-style one as rate_limited.
func TestLedgerErrCarriesCause(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		status   int
		wantSign string
	}{
		{
			name:     "opencode endpoint unavailable",
			body:     `{"error":{"type":"server_error","message":"Upstream request failed: Endpoint is unavailable."}}`,
			status:   http.StatusTooManyRequests,
			wantSign: "[upstream_capacity]",
		},
		{
			name:     "tokenrouter request limit",
			body:     `{"error":{"message":"You have reached the request limit: Maximum 5 requests within 1 minutes.","type":"api_error"}}`,
			status:   http.StatusTooManyRequests,
			wantSign: "[rate_limited]",
		},
		{
			name:     "empty 500 body",
			body:     ``,
			status:   http.StatusInternalServerError,
			wantSign: "[provider_error]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer upstream.Close()

			d := newDispatcher()
			d.SetRetries(0)
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
				strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`))
			rec := httptest.NewRecorder()
			_, res, err := d.ForwardCandidates(context.Background(), rec, req,
				[]Route{testRoute(upstream.URL, provider.SurfaceOpenAI)},
				[]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`))
			if err != nil {
				t.Fatal(err)
			}
			if res.Status != tc.status {
				t.Fatalf("status = %d", res.Status)
			}
			if !strings.Contains(res.Err, tc.wantSign) {
				t.Errorf("ledger err = %q, want the %s cause marker", res.Err, tc.wantSign)
			}
			if tc.body != "" && !strings.Contains(res.Err, strings.Fields(tc.body)[0][:8]) {
				t.Errorf("upstream words lost from err: %q", res.Err)
			}
		})
	}
}
