package registration

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

func TestDoProbeResponseLimit(t *testing.T) {
	const limit = 1 << 20
	for _, tc := range []struct {
		name string
		size int
	}{
		{"at limit", limit},
		{"over limit", limit + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The capped prefix is valid JSON; just validating it would miss the overflow.
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "{}"+strings.Repeat(" ", tc.size-2))
			}))
			defer srv.Close()
			client := srv.Client()
			acct := provider.Account{Kind: provider.KindOpenAICompatible, BaseURL: srv.URL + "/v1"}
			adapter, err := provider.NewRegistry(client).Get(acct.Kind)
			if err != nil {
				t.Fatal(err)
			}
			status, body, err := doProbe(t.Context(), client, adapter, provider.AuthProbe{Path: "/models"}, acct, "test")
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200", status)
			}
			if tc.size > limit {
				if err == nil || !strings.Contains(err.Error(), "exceeds") || body != nil {
					t.Fatalf("oversized response: body length = %d, error = %v", len(body), err)
				}
			} else if err != nil || len(body) != limit {
				t.Fatalf("at limit: body length = %d, error = %v", len(body), err)
			}
		})
	}
}
