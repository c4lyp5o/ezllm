package registration

// The probe chain retries what its comments promise to retry, and the
// protocol sweep never turns weather into a verdict: a 429/5xx leaves the
// stored capability exactly as it was, a 429 stops the sweep, and every
// probe the sweep sends is in the ledger.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// hitCounter counts requests per path.
type hitCounter struct {
	mu   sync.Mutex
	hits map[string]int
}

func (h *hitCounter) inc(path string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.hits == nil {
		h.hits = map[string]int{}
	}
	h.hits[path]++
	return h.hits[path]
}

func (h *hitCounter) get(path string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hits[path]
}

func noSleepTester(client *http.Client, db *store.DB) *Tester {
	t := NewTester(client, provider.NewRegistry(client), db)
	t.sleep = func(context.Context, time.Duration) error { return nil }
	return t
}

func compatAccount(url string) provider.Account {
	return provider.Account{ID: 1, Name: "c", Namespace: "c", Kind: provider.KindOpenAICompatible, BaseURL: url, Enabled: true}
}

// A 503 from the usage oracle is weather: the gate retries it, and the key
// that then answers 200 (with the fake key rejected) is proven.
func TestAuthGateRetriesCapacityStatuses(t *testing.T) {
	var h hitCounter
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/usage") && r.Header.Get("Authorization") != "Bearer sk-real":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
		case strings.HasSuffix(r.URL.Path, "/usage"):
			if h.inc("usage") == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte(`{}`))
		default:
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m1"}]}`))
		}
	}))
	defer srv.Close()
	res := noSleepTester(srv.Client(), nil).Run(context.Background(), compatAccount(srv.URL),
		Options{APIKey: "sk-real", SkipInference: true, ProtocolInline: false})
	if !res.OK {
		t.Fatalf("a 503-then-200 oracle must pass after a retry: fail=%+v steps=%+v", res.Fail, res.Steps)
	}
	// The 503, its retry, then the quota step's own read of /usage.
	if got := h.get("usage"); got != 3 {
		t.Errorf("/usage called %d times with the real key, want 3 (503, retry, quota read)", got)
	}
}

// A provider that keeps answering 429 fails the gate as retryable, and the
// message says what happened instead of ending in an empty error.
func TestAuthGateCapacityFailureNamesTheStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/usage") {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m1"}]}`))
	}))
	defer srv.Close()
	res := noSleepTester(srv.Client(), nil).Run(context.Background(), compatAccount(srv.URL),
		Options{APIKey: "sk-real", SkipInference: true})
	if res.OK || res.Fail == nil || !res.Fail.Retryable {
		t.Fatalf("want a retryable failure, got %+v", res.Fail)
	}
	if !strings.Contains(res.Fail.Message, "HTTP 429") || !strings.Contains(res.Fail.Message, "slow down") {
		t.Errorf("message = %q, want the status and the provider's words", res.Fail.Message)
	}
}

// The inference gate retries a 503 too; a model that answers on the second
// try proves a key whose catalog and oracle could not.
func TestInferenceRetriesCapacityStatuses(t *testing.T) {
	var h hitCounter
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/usage"):
			w.WriteHeader(http.StatusNotFound)
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			if h.inc("chat") == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
		default: // public catalog: 200 for any key, proves nothing
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m1"}]}`))
		}
	}))
	defer srv.Close()
	res := noSleepTester(srv.Client(), nil).Run(context.Background(), compatAccount(srv.URL), Options{APIKey: "sk-real"})
	if !res.OK {
		t.Fatalf("a 503-then-200 generation must prove the key: fail=%+v steps=%+v", res.Fail, res.Steps)
	}
	if got := h.get("chat"); got != 2 {
		t.Errorf("generation called %d times, want 2", got)
	}
}

func sweepDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), store.Options{
		Path: filepath.Join(t.TempDir(), "t.sqlite"), BatchSize: 1, BatchWait: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// A capacity answer during a re-sweep is not evidence: the verdict proven
// earlier stays, in the store and in the rows the caller persists again.
func TestSweepNeverOverwritesAVerdictWithWeather(t *testing.T) {
	db := sweepDB(t)
	ctx := context.Background()
	acctID, err := db.UpsertAccount(ctx, provider.Account{Name: "c", Namespace: "c", Kind: provider.KindOpenAICompatible, BaseURL: "http://x", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	yes := true
	if err := db.SetModelProto(ctx, acctID, "m1", provider.SurfaceOpenAI, &yes); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	acct := compatAccount(srv.URL)
	acct.ID = acctID
	res := noSleepTester(srv.Client(), db).ProtocolSweep(ctx, acct, "k", []provider.ModelInfo{{ID: "m1"}},
		Options{Surfaces: []provider.Surface{provider.SurfaceOpenAI}})
	if _, has := res.Rows["m1"][string(provider.SurfaceOpenAI)]; has {
		t.Errorf("an unknown verdict entered the persisted rows: %+v", res.Rows)
	}
	got, err := db.ModelSupportsSurface(ctx, acctID, "m1", provider.SurfaceOpenAI)
	if err != nil || got == nil || !*got {
		t.Fatalf("proven openai support became %v (err %v) after a 503", got, err)
	}
}

// A 429 is the provider asking us to stop: the sweep stops instead of
// spending the rest of the catalog against a rate limit. A 5xx is one
// model's trouble, and the sweep moves on.
func TestSweepStopsOnRateLimitButNotOnServerError(t *testing.T) {
	for _, tc := range []struct {
		status    int
		wantCalls int
	}{
		{http.StatusTooManyRequests, 1},
		{http.StatusBadGateway, 3},
	} {
		var h hitCounter
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.inc("post")
			w.WriteHeader(tc.status)
		}))
		models := []provider.ModelInfo{{ID: "m1"}, {ID: "m2"}, {ID: "m3"}}
		noSleepTester(srv.Client(), nil).ProtocolSweep(context.Background(), compatAccount(srv.URL), "k", models,
			Options{Surfaces: []provider.Surface{provider.SurfaceOpenAI}, ProbeAll: true})
		srv.Close()
		if got := h.get("post"); got != tc.wantCalls {
			t.Errorf("HTTP %d: %d probes, want %d", tc.status, got, tc.wantCalls)
		}
	}
}

// Every sweep probe is a real generation call: each one is ledgered, like
// the inference probe, so the spend is never invisible.
func TestSweepProbesAreLedgered(t *testing.T) {
	db := sweepDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()
	noSleepTester(srv.Client(), db).ProtocolSweep(context.Background(), compatAccount(srv.URL), "k",
		[]provider.ModelInfo{{ID: "m1"}, {ID: "m2"}},
		Options{Surfaces: []provider.Surface{provider.SurfaceOpenAI, provider.SurfaceResponses}})
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}
	calls, err := db.RecentCalls(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, c := range calls {
		if c.Client == "admin:test" && c.Status == http.StatusOK {
			n++
		}
	}
	if n != 4 {
		t.Errorf("ledgered %d sweep probes, want 4 (2 models x 2 surfaces): %+v", n, calls)
	}
}

// claude-web is served through ezllm's own chat-completions bridge: the
// openai wire is the ONLY one it has. The sweep must probe it, never mark it
// impossible.
func TestClaudeWebKeepsItsOpenAIWire(t *testing.T) {
	if isAnthropicOnlyKind(provider.KindClaudeWeb) {
		t.Fatal("claude-web serves /chat/completions; it is not anthropic-only")
	}
	if !isAnthropicOnlyKind(provider.KindAnthropicCompat) {
		t.Fatal("anthropic-compatible accounts still skip the openai probe")
	}
}

// The sweep must not reorder or shrink the caller's surface list in place.
func TestSweepLeavesCallerSurfacesIntact(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	surfaces := []provider.Surface{provider.SurfaceOpenAI, provider.SurfaceAnthropic, provider.SurfaceResponses}
	acct := compatAccount(srv.URL)
	acct.Kind = provider.KindAnthropicCompat
	noSleepTester(srv.Client(), nil).ProtocolSweep(context.Background(), acct, "k", []provider.ModelInfo{{ID: "m1"}},
		Options{Surfaces: surfaces})
	want := []provider.Surface{provider.SurfaceOpenAI, provider.SurfaceAnthropic, provider.SurfaceResponses}
	for i := range want {
		if surfaces[i] != want[i] {
			t.Fatalf("caller's surfaces changed to %v", surfaces)
		}
	}
}
