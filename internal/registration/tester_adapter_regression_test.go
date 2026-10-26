package registration

// Regression test for the adapter hijack: doProbe used to resolve its adapter
// from a PACKAGE-LEVEL var. The server shares ONE Tester across every request
// and spawns background ProtocolSweeps, so two overlapping key tests could
// prepare each other's probes — the wrong auth header (Bearer vs x-api-key),
// the wrong strip list, the wrong oracle path — and reject a valid key.
//
// This test parks key test A mid-run, starts key test B against a different
// adapter kind, lets A finish, and asserts that every probe A made was
// prepared by A's own adapter. B is still parked in its own first
// PrepareRequest for the whole window, so a single "B" entry would prove a
// hijack. (go test -race does NOT catch this: the channel handshake creates
// happens-before edges; only the logical attribution detects it.)

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// anyServer answers 200 JSON everywhere so the oracle step always succeeds
// and the flow advances to calibration and inference probes.
func anyServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m1"}]}`))
	}))
}

type callLog struct {
	mu    sync.Mutex
	order []string
}

func (l *callLog) add(tag string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.order = append(l.order, tag)
}

func (l *callLog) count(tag string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, v := range l.order {
		if v == tag {
			n++
		}
	}
	return n
}

func (l *callLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.order))
	copy(out, l.order)
	return out
}

// gateAdapter blocks inside PrepareRequest on its FIRST call only, and records
// the tag only AFTER the block clears. So while B is parked, any "B" entry in
// the log can only have been produced by a caller other than B's own key test.
type gateAdapter struct {
	kind    provider.Kind
	tag     string
	enter   chan struct{}
	release chan struct{}
	log     *callLog

	mu      sync.Mutex
	blocked bool
}

func (a *gateAdapter) Kind() provider.Kind { return a.kind }

func (a *gateAdapter) PrepareRequest(ctx context.Context, req *http.Request, acct provider.Account, key string, s provider.Surface) error {
	// Only the very first call parks. A deliberately-avoided sync.Once here:
	// Once would make a hijacked call serialise behind the parked one,
	// hiding the defect behind a harness artefact.
	a.mu.Lock()
	first := !a.blocked
	if first {
		a.blocked = true
	}
	a.mu.Unlock()
	if first {
		a.enter <- struct{}{}
		<-a.release
	}
	a.log.add(a.tag)
	req.Header.Set("X-Probe-Adapter", a.tag)
	return nil
}

func (a *gateAdapter) StripHeaders() []string { return nil }
func (a *gateAdapter) CatalogIsPublic() bool  { return false }
func (a *gateAdapter) AuthOracle() provider.AuthProbe {
	return provider.AuthProbe{Method: http.MethodGet, Path: "/usage"}
}
func (a *gateAdapter) ReadQuota(context.Context, provider.Account, string) (provider.Quota, error) {
	return provider.Quota{}, provider.ErrQuotaUnsupported
}
func (a *gateAdapter) ListModels(context.Context, provider.Account, string) ([]provider.ModelInfo, error) {
	return []provider.ModelInfo{{ID: "m1"}}, nil
}

const (
	kindA = provider.Kind("evidence-kind-a")
	kindB = provider.Kind("evidence-kind-b")
)

// TestProbesArePreparedByTheirOwnAdapter is the regression guard: a shared
// Tester must never resolve its adapter process-wide.
func TestProbesArePreparedByTheirOwnAdapter(t *testing.T) {
	srv := anyServer(t)
	defer srv.Close()

	log := &callLog{}
	adapterA := &gateAdapter{kind: kindA, tag: "A", enter: make(chan struct{}, 8), release: make(chan struct{}), log: log}
	adapterB := &gateAdapter{kind: kindB, tag: "B", enter: make(chan struct{}, 8), release: make(chan struct{}), log: log}

	reg := provider.NewRegistry(srv.Client())
	reg.Register(adapterA)
	reg.Register(adapterB)

	// ONE shared Tester, exactly as server.go builds it (registration.NewTester)
	// for the whole server. This is the condition that made the old global
	// dangerous.
	tester := NewTester(srv.Client(), reg, nil)

	acctA := provider.Account{ID: 1, Namespace: "acct-a", Kind: kindA, BaseURL: srv.URL + "/v1", Enabled: true}
	acctB := provider.Account{ID: 2, Namespace: "acct-b", Kind: kindB, BaseURL: srv.URL + "/v1", Enabled: true}

	opts := func() Options {
		return Options{Label: "evidence", APIKey: "«reda...…»", ProtocolInline: false}
	}

	doneA := make(chan struct{})
	doneB := make(chan struct{})

	// Key test A starts and parks inside its auth-oracle probe (adapter A).
	go func() { defer close(doneA); _ = tester.Run(context.Background(), acctA, opts()) }()
	select {
	case <-adapterA.enter:
	case <-time.After(5 * time.Second):
		t.Fatal("key test A never reached its oracle probe")
	}

	// Key test B starts on the SAME Tester and parks inside its own first
	// PrepareRequest. Under the defect, A's next probe would still reach
	// adapter B, because B's registration overwrote the shared global.
	go func() { defer close(doneB); _ = tester.Run(context.Background(), acctB, opts()) }()
	select {
	case <-adapterB.enter:
	case <-time.After(5 * time.Second):
		t.Fatal("key test B never reached its oracle probe")
	}

	// Unpark A. Its calibration and inference probes must run under
	// adapter A. B is still parked and has therefore logged NOTHING yet:
	// any "B" in the log is attributable only to A's run — the hijack.
	close(adapterA.release)

	select {
	case <-doneA:
	case <-time.After(30 * time.Second):
		t.Fatalf("key test A did not finish (order=%v)", log.snapshot())
	}

	// B is still parked: nothing of B's has completed.
	select {
	case <-doneB:
		t.Fatal("key test B should still be parked — a completed B would make attribution meaningless")
	default:
	}

	order := log.snapshot()
	if bCount := log.count("B"); bCount != 0 {
		t.Fatalf("HIJACK: adapter B prepared %d probe(s) for key test A while B was still parked (order=%v)", bCount, order)
	}
	// A must have made at least its oracle and its fake-key calibration
	// probe, both under adapter A. Two entries are the floor: a single
	// entry would mean A was short-circuited and the test is vacuous.
	if aCount := log.count("A"); aCount < 2 {
		t.Fatalf("key test A produced %d probe entr(ies) (%v); expected at least oracle + calibration", aCount, order)
	}
	for i, tag := range order {
		if tag != "A" {
			t.Fatalf("probe #%d was prepared by adapter %q, not A (order=%v)", i+1, tag, order)
		}
	}

	// Unpark B so the goroutines can exit cleanly.
	close(adapterB.release)
	select {
	case <-doneB:
	case <-time.After(30 * time.Second):
		t.Fatal("key test B did not finish after release")
	}
}
