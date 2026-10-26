package registration

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// Probes — the key test and the protocol sweep — are admin traffic, not client
// traffic: they must leave no row in the usage ledger, or every key test and
// sweep inflates the request counts and spend the dashboards report.
func newProbeTestStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), store.Options{Path: t.TempDir() + "/t.sqlite"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// deniedProbeServer refuses every probe with the body shape that a real
// provider uses for a model outside the plan.
func deniedProbeServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"message":"Access to model denied. Please make sure your account has access."}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func assertLedgerEmpty(t *testing.T, db *store.DB) {
	t.Helper()
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}
	rows, err := db.RecentCalls(context.Background(), 50)
	if err != nil {
		t.Fatalf("RecentCalls: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("a probe reached the ledger: %+v", rows)
	}
}

func TestKeyTestProbeIsNotLedgered(t *testing.T) {
	db := newProbeTestStore(t)
	srv := deniedProbeServer(t)
	tester := NewTester(srv.Client(), provider.NewRegistry(srv.Client()), db)
	acct := provider.Account{
		Name: "t", Namespace: "probe-ns", Kind: provider.KindOpenAICompatible,
		BaseURL: srv.URL, Enabled: true,
	}
	adapter, err := tester.registry.Get(acct.Kind)
	if err != nil {
		t.Fatalf("adapter for %q: %v", acct.Kind, err)
	}

	tester.probeModel(context.Background(), adapter, acct, "k", "m1", &pacer{}, &TestResult{})

	assertLedgerEmpty(t, db)
}

func TestProtocolProbeIsNotLedgered(t *testing.T) {
	db := newProbeTestStore(t)
	srv := deniedProbeServer(t)
	tester := NewTester(srv.Client(), provider.NewRegistry(srv.Client()), db)
	acct := provider.Account{
		Name: "t", Namespace: "probe-ns", Kind: provider.KindOpenAICompatible,
		BaseURL: srv.URL, Enabled: true,
	}

	tester.ProtocolSweep(context.Background(), acct, "k",
		[]provider.ModelInfo{{ID: "m1"}},
		Options{Surfaces: []provider.Surface{provider.SurfaceOpenAI}, ProbeAll: true})

	assertLedgerEmpty(t, db)
}
