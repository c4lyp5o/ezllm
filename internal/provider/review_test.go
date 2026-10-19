package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// A gateway with no /usage (Groq, OpenAI, NVIDIA, Cloudflare, …) has no quota
// endpoint: that is ErrQuotaUnsupported, which the refresh records as "none",
// not a retryable upstream failure. A 5xx or a credential rejection is still
// an error, so a transient fault never overwrites a good snapshot.
func TestGenericUsageWithoutEndpointIsUnsupported(t *testing.T) {
	for _, tc := range []struct {
		status      int
		unsupported bool
	}{
		{http.StatusNotFound, true},
		{http.StatusMethodNotAllowed, true},
		{http.StatusBadRequest, true}, // OpenAI: GET /v1/usage → 400 "missing date"
		{http.StatusInternalServerError, false},
		{http.StatusUnauthorized, false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
		}))
		for _, a := range []Adapter{NewOpenAICompatible(srv.Client()), NewOpenCodeGo(srv.Client())} {
			_, err := a.ReadQuota(context.Background(), Account{Kind: a.Kind(), BaseURL: srv.URL + "/v1"}, "k")
			if got := errors.Is(err, ErrQuotaUnsupported); got != tc.unsupported {
				t.Errorf("%s HTTP %d: unsupported=%v (err %v), want %v", a.Kind(), tc.status, got, err, tc.unsupported)
			}
		}
		srv.Close()
	}
}

// fakeHosts answers every request in-process, whatever the host, and records
// what was asked.
type fakeHosts struct {
	mu   sync.Mutex
	reqs []*http.Request
	body string
}

func (f *fakeHosts) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, r)
	f.mu.Unlock()
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "application/json")
	_, _ = rec.WriteString(f.body)
	return rec.Result(), nil
}

// TokenRouter's wallet lives on its management API and takes the management
// key only. Without one there is nothing to read — and calling with the
// inference key draws a 401 that a key test reads as a dead key.
func TestTokenRouterQuotaNeedsTheManagementKey(t *testing.T) {
	f := &fakeHosts{body: `{"data":{"topUpBalance":3.5},"success":true}`}
	a := NewOpenAICompatible(&http.Client{Transport: f})
	acct := Account{Kind: KindOpenAICompatible, BaseURL: "https://api.tokenrouter.com/v1"}

	if _, err := a.ReadQuota(context.Background(), acct, "sk-inference"); !errors.Is(err, ErrQuotaUnsupported) {
		t.Fatalf("no management key: err = %v, want ErrQuotaUnsupported", err)
	}
	if len(f.reqs) != 0 {
		t.Fatalf("no management key must mean no call; got %d", len(f.reqs))
	}
	acct.ManagementKey = "sk-mgmt"
	if q, err := a.ReadQuota(context.Background(), acct, "sk-inference"); err != nil || q.Kind != QuotaMoney {
		t.Fatalf("with a management key: %+v %v", q, err)
	}
	if got := f.reqs[0].Header.Get("Authorization"); got != "Bearer sk-mgmt" {
		t.Errorf("wallet auth = %q, want the management key", got)
	}
}

// The management key goes to TokenRouter's own host only. A base URL that
// merely CONTAINS the hostname gets the inference key, like any gateway.
func TestManagementKeyNeverLeavesTokenRouter(t *testing.T) {
	f := &fakeHosts{body: `{"balance":1}`}
	a := NewOpenAICompatible(&http.Client{Transport: f})
	for _, base := range []string{"https://api.tokenrouter.com.evil.test/v1", "https://evil.test/api.tokenrouter.com/v1"} {
		f.reqs = nil
		acct := Account{Kind: KindOpenAICompatible, BaseURL: base, ManagementKey: "sk-mgmt"}
		_, _ = a.ReadQuota(context.Background(), acct, "sk-inference")
		for _, r := range f.reqs {
			if strings.Contains(r.Header.Get("Authorization"), "sk-mgmt") {
				t.Errorf("%s: management key sent to %s", base, r.URL)
			}
		}
	}
}

// An account's custom_headers reach every request ezllm sends for it —
// catalog and quota included, not only inference and probes. A gateway that
// needs an org or tenant header otherwise lists nothing.
func TestCatalogAndQuotaCarryCustomHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Tenant") != "acme" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/models"):
			_, _ = w.Write([]byte(`{"data":[{"id":"m1"}]}`))
		default:
			_, _ = w.Write([]byte(`{"balance":2.5}`))
		}
	}))
	defer srv.Close()
	hdrs := map[string]string{"X-Tenant": "acme"}
	for _, a := range []Adapter{NewOpenAICompatible(srv.Client()), NewOpenCodeGo(srv.Client()), NewAnthropicCompatible(srv.Client())} {
		acct := Account{Kind: a.Kind(), BaseURL: srv.URL + "/v1", CustomHeaders: hdrs}
		if ms, err := a.ListModels(context.Background(), acct, "k"); err != nil || len(ms) != 1 {
			t.Errorf("%s catalog: %v %v", a.Kind(), ms, err)
		}
		if a.Kind() == KindAnthropicCompat {
			continue // no quota endpoint on this kind
		}
		if q, err := a.ReadQuota(context.Background(), acct, "k"); err != nil || q.Kind != QuotaMoney {
			t.Errorf("%s quota: %+v %v", a.Kind(), q, err)
		}
	}
}

// Anthropic's /v1/models is paginated (20 per page by default). The catalog
// follows has_more/last_id to the end instead of keeping the first page.
func TestAnthropicCatalogFollowsPages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("after_id") {
		case "":
			_, _ = w.Write([]byte(`{"data":[{"id":"a"},{"id":"b"}],"has_more":true,"last_id":"b"}`))
		case "b":
			_, _ = w.Write([]byte(`{"data":[{"id":"c"}],"has_more":false,"last_id":"c"}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	ms, err := NewAnthropicCompatible(srv.Client()).ListModels(context.Background(),
		Account{Kind: KindAnthropicCompat, BaseURL: srv.URL}, "k")
	if err != nil || len(ms) != 3 || ms[2].ID != "c" {
		t.Fatalf("models = %+v, err %v; want a, b, c", ms, err)
	}
}

// A provider error quoted in a message is cut on a rune boundary: the text
// lands in the ledger and the dashboard, and a half rune is invalid UTF-8.
func TestHTTPErrorTruncatesOnRuneBoundary(t *testing.T) {
	body := strings.Repeat("a", 199) + "é" + strings.Repeat("z", 50)
	msg := (&HTTPError{URL: "u", Status: 500, Body: body}).Error()
	if !utf8.ValidString(msg) {
		t.Fatalf("message is not valid UTF-8: %q", msg)
	}
}

// A base URL saved with a trailing slash still reaches {base}/usage, not
// {base}//usage (which most servers 404).
func TestQuotaPathToleratesTrailingSlash(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = fmt.Fprint(w, `{"balance":1}`)
	}))
	defer srv.Close()
	for _, a := range []Adapter{NewOpenAICompatible(srv.Client()), NewOpenCodeGo(srv.Client())} {
		path = ""
		_, _ = a.ReadQuota(context.Background(), Account{Kind: a.Kind(), BaseURL: srv.URL + "/v1/"}, "k")
		if path != "/v1/usage" {
			t.Errorf("%s: requested %q, want /v1/usage", a.Kind(), path)
		}
	}
}
