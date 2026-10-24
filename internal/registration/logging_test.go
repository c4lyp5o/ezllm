package registration

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/logx"
	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/store"
)

type syncSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func learnStore(t *testing.T) *store.DB {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, store.Options{Path: t.TempDir() + "/t.sqlite"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// A learned vocabulary changes how every later request is sent, so it belongs
// on the console at Info — once, when it is learned.
func TestSetReasoningLogsWhatItLearned(t *testing.T) {
	ctx := context.Background()
	db := learnStore(t)
	acct := provider.Account{Name: "oc", Namespace: "oc", Kind: provider.KindOpenAICompatible, BaseURL: "https://x.invalid", Enabled: true}
	aid, err := db.UpsertAccount(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertModels(ctx, aid, []provider.ModelInfo{{ID: "m"}}); err != nil {
		t.Fatal(err)
	}

	sink := &syncSink{}
	tester := NewTester(nil, nil, db)
	tester.SetLogger(slog.New(logx.NewHandler(sink, logx.Options{Level: slog.LevelDebug})))
	tester.setReasoning(aid, "m", provider.ReasoningRule{
		Allowed: []string{"minimal", "low", "max"}, Known: true,
	})

	out := sink.String()
	if !strings.Contains(out, "reasoning vocabulary learned") {
		t.Fatalf("learning was silent:\n%s", out)
	}
	if !strings.Contains(out, "minimal,low,max") || !strings.Contains(out, "model=m") {
		t.Fatalf("the line does not name what was learned:\n%s", out)
	}
}

// The case the store cannot report any other way: the write matched no catalog
// row, so the rule is gone. Debug, because a combo hop hits this every probe.
func TestSetReasoningReportsAnUncataloguedModel(t *testing.T) {
	ctx := context.Background()
	db := learnStore(t)
	acct := provider.Account{Name: "oc", Namespace: "oc", Kind: provider.KindOpenAICompatible, BaseURL: "https://x.invalid", Enabled: true}
	aid, err := db.UpsertAccount(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}

	sink := &syncSink{}
	tester := NewTester(nil, nil, db)
	tester.SetLogger(slog.New(logx.NewHandler(sink, logx.Options{Level: slog.LevelDebug})))
	tester.setReasoning(aid, "not-in-catalog", provider.ReasoningRule{Allowed: []string{"low"}, Known: true})

	out := sink.String()
	if !strings.Contains(out, "not kept") {
		t.Fatalf("a dropped vocabulary was silent:\n%s", out)
	}
	if strings.Contains(out, "INFO") {
		t.Fatalf("the dropped case is routine and must stay at Debug:\n%s", out)
	}
}

// At Info the dropped case vanishes: an operator sees only facts that stuck.
func TestDroppedVocabularyIsInvisibleAtInfo(t *testing.T) {
	ctx := context.Background()
	db := learnStore(t)
	acct := provider.Account{Name: "oc", Namespace: "oc", Kind: provider.KindOpenAICompatible, BaseURL: "https://x.invalid", Enabled: true}
	aid, err := db.UpsertAccount(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	sink := &syncSink{}
	tester := NewTester(nil, nil, db)
	tester.SetLogger(slog.New(logx.NewHandler(sink, logx.Options{Level: slog.LevelInfo})))
	tester.setReasoning(aid, "not-in-catalog", provider.ReasoningRule{Allowed: []string{"low"}, Known: true})
	if out := sink.String(); strings.Contains(out, "not kept") {
		t.Fatalf("a Debug line leaked at Info:\n%s", out)
	}
}

// One shared tester across concurrent sweeps: SetLogger must be safe to call
// before the sweeps start, and the zero value must still log rather than panic.
func TestZeroValueTesterStillLogs(t *testing.T) {
	var tester Tester
	if tester.logf() == nil {
		t.Fatal("a hand-built Tester must fall back to slog.Default")
	}
	tester.SetLogger(nil)
	if tester.logf() == nil {
		t.Fatal("SetLogger(nil) must restore a usable logger")
	}
}

// A 400 that names the wrong endpoint does not fail the key: it TEACHES the
// router where the model lives, and that reroute belongs on the console.
func TestWireRecordedIsLoggedWhenAMismatchProbeSucceeds(t *testing.T) {
	ctx := context.Background()
	db := learnStore(t)
	tr := &copilotTransport{chatStatus: http.StatusBadRequest}
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	acct := provider.Account{
		Name: "gh-copilot", Namespace: "copilot", Kind: provider.KindGitHubCopilot,
		BaseURL: "https://api.githubcopilot.com", Enabled: true,
	}
	aid, err := db.UpsertAccount(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	acct.ID = aid
	if err := db.UpsertModels(ctx, aid, []provider.ModelInfo{{ID: "gpt-6.1-sol"}}); err != nil {
		t.Fatal(err)
	}

	sink := &syncSink{}
	tester := NewTester(client, provider.NewRegistry(client), db)
	tester.SetLogger(slog.New(logx.NewHandler(sink, logx.Options{Level: slog.LevelInfo})))
	res := tester.Run(ctx, acct, Options{Label: "copilot", APIKey: "gho_real"})
	if !res.OK {
		t.Fatalf("the responses wire answered; fail = %+v", res.Fail)
	}
	out := sink.String()
	if !strings.Contains(out, "wire recorded") {
		t.Fatalf("the wire change was silent:\n%s", out)
	}
	if !strings.Contains(out, "account=copilot") || !strings.Contains(out, "model=gpt-") {
		t.Fatalf("the line does not name the model and account:\n%s", out)
	}
}
