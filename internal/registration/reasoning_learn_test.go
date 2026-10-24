package registration

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// opencode-go's refusal, verbatim from the ledger.
const sparkReasoningRefusal = `{"model":"muse-spark-1.3-contributor","error":{"code":null,"message":"reasoning_effort 'none' is not supported for model 'muse-spark-1.3-contributor'. Supported values: [minimal, low, medium, high, xhigh, max]","param":"reasoning.effort","type":"invalid_request_error"}}`

// A key test is an admin action someone already paid for. If a probe is refused
// for its reasoning vocabulary, that answer must be kept: the next real request
// is then clamped instead of refused.
func TestProbeFailureTeachesTheReasoningVocabulary(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, store.Options{Path: t.TempDir() + "/t.sqlite"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	srv := &reasoningStub{}
	client := &http.Client{Transport: srv, Timeout: 10 * time.Second}
	acct := provider.Account{
		Name: "oc", Namespace: "oc", Kind: provider.KindOpenAICompatible,
		BaseURL: "https://up.invalid", Enabled: true,
	}
	aid, err := db.UpsertAccount(ctx, acct)
	if err != nil {
		t.Fatalf("seed account: %v", err)
	}
	acct.ID = aid
	if err := db.UpsertModels(ctx, aid, []provider.ModelInfo{{ID: "muse-spark"}}); err != nil {
		t.Fatalf("seed catalog: %v", err)
	}

	tester := NewTester(client, provider.NewRegistry(client), db)
	adapter, err := tester.registry.Get(acct.Kind)
	if err != nil {
		t.Fatalf("adapter: %v", err)
	}
	tester.probeModel(ctx, adapter, acct, "k", "muse-spark", &pacer{}, &TestResult{})

	facts, err := db.ModelFacts(ctx, aid, "muse-spark")
	if err != nil {
		t.Fatalf("facts: %v", err)
	}
	if !facts.Reasoning.Known || len(facts.Reasoning.Allowed) != 6 {
		t.Fatalf("the probe did not record the vocabulary it was shown: %+v", facts.Reasoning)
	}
	// And what it recorded is what the router will clamp with.
	if v, drop, changed := facts.Reasoning.Clamp("none"); drop || !changed || v != "minimal" {
		t.Fatalf("Clamp(none) = (%q, drop=%t, changed=%t), want minimal", v, drop, changed)
	}
}

// A probe refused for something ELSE must not invent a vocabulary.
func TestProbeFailureForAnotherReasonTeachesNothing(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, store.Options{Path: t.TempDir() + "/t.sqlite"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	srv := &reasoningStub{body: `{"error":{"message":"context length exceeded"}}`}
	client := &http.Client{Transport: srv, Timeout: 10 * time.Second}
	acct := provider.Account{
		Name: "oc", Namespace: "oc", Kind: provider.KindOpenAICompatible,
		BaseURL: "https://up.invalid", Enabled: true,
	}
	aid, err := db.UpsertAccount(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	acct.ID = aid
	if err := db.UpsertModels(ctx, aid, []provider.ModelInfo{{ID: "m"}}); err != nil {
		t.Fatal(err)
	}

	tester := NewTester(client, provider.NewRegistry(client), db)
	adapter, _ := tester.registry.Get(acct.Kind)
	tester.probeModel(ctx, adapter, acct, "k", "m", &pacer{}, &TestResult{})

	facts, _ := db.ModelFacts(ctx, aid, "m")
	if facts.Reasoning.Known {
		t.Fatalf("invented a vocabulary from an unrelated 400: %+v", facts.Reasoning)
	}
}

// reasoningStub refuses every inference call with the configured body.
type reasoningStub struct{ body string }

func (s *reasoningStub) RoundTrip(r *http.Request) (*http.Response, error) {
	reply := func(status int, body string) (*http.Response, error) {
		h := make(http.Header)
		h.Set("Content-Type", "application/json")
		return &http.Response{StatusCode: status, Header: h,
			Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/models"):
		return reply(http.StatusOK, `{"data":[{"id":"muse-spark"}]}`)
	case strings.HasSuffix(r.URL.Path, "/usage"):
		return reply(http.StatusNotFound, `{}`)
	}
	body := s.body
	if body == "" {
		body = sparkReasoningRefusal
	}
	return reply(http.StatusBadRequest, body)
}

func TestProtocolSweepLearnsReasoningWithoutDisablingWire(t *testing.T) {
	for _, surface := range []provider.Surface{provider.SurfaceOpenAI, provider.SurfaceAnthropic, provider.SurfaceResponses} {
		for _, proven := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/proven=%t", surface, proven), func(t *testing.T) {
				ctx := context.Background()
				db := newProbeTestStore(t)
				acct := provider.Account{Name: "oc", Namespace: "oc", Kind: provider.KindOpenAICompatible, BaseURL: "https://up.invalid", Enabled: true}
				aid, err := db.UpsertAccount(ctx, acct)
				if err != nil {
					t.Fatal(err)
				}
				acct.ID = aid
				if err := db.UpsertModels(ctx, aid, []provider.ModelInfo{{ID: "muse-spark"}}); err != nil {
					t.Fatal(err)
				}
				if proven {
					yes := true
					if err := db.SetModelProto(ctx, aid, "muse-spark", surface, &yes); err != nil {
						t.Fatal(err)
					}
				}
				client := &http.Client{Transport: &reasoningStub{}}
				tester := NewTester(client, provider.NewRegistry(client), db)
				result := tester.ProtocolSweep(ctx, acct, "k", []provider.ModelInfo{{ID: "muse-spark"}}, Options{Surfaces: []provider.Surface{surface}, ProbeAll: true})
				facts, err := db.ModelFacts(ctx, aid, "muse-spark")
				if err != nil {
					t.Fatal(err)
				}
				if !facts.Reasoning.Known || len(facts.Reasoning.Allowed) != 6 {
					t.Errorf("sweep lost vocabulary: %+v", facts.Reasoning)
				}
				support := facts.Supports(surface)
				if proven && (support == nil || !*support) {
					t.Error("sweep erased proven wire support")
				}
				if !proven && support != nil {
					t.Error("a reasoning refusal must leave untested support unknown")
				}
				if len(result.Rows) != 0 {
					t.Errorf("reasoning refusal produced wire verdicts: %+v", result.Rows)
				}
			})
		}
	}
}
