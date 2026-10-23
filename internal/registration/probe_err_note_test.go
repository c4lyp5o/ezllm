package registration

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// A probe that fails must record WHY in the ledger. Before this, 183 of the
// 189 error rows in the 48h window ending 2026-10-23 carried a status and an
// empty err, so a bare 400 could not distinguish "the plan does not include
// this model" from a malformed probe — the one thing an operator needs.
//
// The key test and the protocol sweep each write their own row, so each is
// asserted separately: a fix applied to one must not hide behind the other.
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

// waitProbeRow polls the batched ledger for the probe row this test caused.
func waitProbeRow(t *testing.T, db *store.DB, status int) store.Call {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows, err := db.RecentCalls(ctx, 50)
		if err != nil {
			t.Fatalf("RecentCalls: %v", err)
		}
		for _, c := range rows {
			if c.Client == "admin:test" && c.Status == status {
				return c
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no admin:test row with status %d reached the ledger (%d rows seen)", status, len(rows))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func assertProbeReason(t *testing.T, c store.Call) {
	t.Helper()
	if !strings.Contains(c.Err, "upstream 403") {
		t.Fatalf("probe row has no upstream reason; err = %q", c.Err)
	}
	if !strings.Contains(c.Err, "Access to model denied") {
		t.Fatalf("probe row dropped the provider's own words; err = %q", c.Err)
	}
}

func TestKeyTestProbeRecordsItsReason(t *testing.T) {
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

	assertProbeReason(t, waitProbeRow(t, db, http.StatusForbidden))
}

func TestProtocolProbeRecordsItsReason(t *testing.T) {
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

	assertProbeReason(t, waitProbeRow(t, db, http.StatusForbidden))
}

// The pure renderer's edges: a transport error, a success, an empty body, and a
// body long enough to need the bound.
func TestProbeErrNote(t *testing.T) {
	if got := probeErrNote(0, nil, io.ErrUnexpectedEOF); !strings.Contains(got, "probe transport") {
		t.Errorf("transport error note = %q, want a transport prefix", got)
	}
	if got := probeErrNote(200, []byte(`{"ok":true}`), nil); got != "" {
		t.Errorf("a successful probe must record nothing, got %q", got)
	}
	if got := probeErrNote(502, nil, nil); !strings.Contains(got, "(empty body)") {
		t.Errorf("empty-body note = %q, want an explicit marker", got)
	}
	long := probeErrNote(400, []byte(strings.Repeat("x", maxProbeErrNote*2)), nil)
	if len(long) > maxProbeErrNote+80 {
		t.Errorf("note is unbounded: %d bytes", len(long))
	}
	if !strings.Contains(long, "…") {
		t.Errorf("a truncated note should say so")
	}
}
