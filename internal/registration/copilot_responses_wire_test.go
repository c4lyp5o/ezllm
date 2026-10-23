package registration

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// A wire mismatch is a signpost: the provider names the endpoint the model
// lives on. Probe the same model once on /responses, and when it answers,
// record the wire so routing stops needing a hand-written regex.
func TestEndpointMismatchProbesTheResponsesWire(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, store.Options{Path: t.TempDir() + "/t.sqlite"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	tr := &copilotTransport{chatStatus: http.StatusBadRequest}
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	acct := provider.Account{
		Name: "gh-copilot", Namespace: "copilot", Kind: provider.KindGitHubCopilot,
		BaseURL: "https://api.githubcopilot.com", Enabled: true,
	}
	aid, err := db.UpsertAccount(ctx, acct)
	if err != nil {
		t.Fatalf("seed account: %v", err)
	}
	acct.ID = aid
	if err := db.UpsertModels(ctx, aid, []provider.ModelInfo{{ID: "gpt-6.1-sol"}}); err != nil {
		t.Fatalf("seed catalog: %v", err)
	}

	tester := NewTester(client, provider.NewRegistry(client), db)
	res := tester.Run(ctx, acct, Options{Label: "copilot", APIKey: copilotRealKey})

	if !res.OK {
		t.Fatalf("the responses wire answered, so the key test must pass; fail = %+v", res.Fail)
	}
	if tr.responses == 0 {
		t.Fatalf("the mismatch must trigger a /responses probe, none happened")
	}
	var sawWire bool
	for _, s := range res.Steps {
		if s.Step == StepInference && strings.Contains(s.Detail, "answers on /responses") {
			sawWire = true
		}
	}
	if !sawWire {
		t.Fatalf("the inference step must say the model answered on /responses; steps = %+v", res.Steps)
	}

	// The capability record is the point: the hop now knows which wire works.
	// The probe picks its own candidate from the catalog (the cheap one, not the
	// first entry), so the assertion follows the verdict, not the model name.
	rows, err := db.ListModels(ctx, aid)
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	var probed []store.ModelEntryRow
	for _, r := range rows {
		if r.Source == "probe" {
			probed = append(probed, r)
		}
	}
	if len(probed) != 1 {
		t.Fatalf("expected exactly one probed model, got %d (%d rows)", len(probed), len(rows))
	}
	m := probed[0]
	if m.OpenAI == nil || *m.OpenAI {
		t.Fatalf("%s: proto_openai must be recorded FALSE (the chat wire refused), got %v", m.ID, m.OpenAI)
	}
	if m.Responses == nil || !*m.Responses {
		t.Fatalf("%s: proto_responses must be recorded TRUE, got %v", m.ID, m.Responses)
	}
}

// The control: a RETIRED model is not a wire mismatch. Retrying it on another
// wire would spend a call to learn the same thing, so /responses is never
// dialled and nothing is recorded.
func TestRetiredModelDoesNotProbeAnotherWire(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, store.Options{Path: t.TempDir() + "/t.sqlite"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	tr := &retiredTransport{}
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	acct := provider.Account{
		Name: "gh-copilot", Namespace: "copilot", Kind: provider.KindGitHubCopilot,
		BaseURL: "https://api.githubcopilot.com", Enabled: true,
	}
	aid, err := db.UpsertAccount(ctx, acct)
	if err != nil {
		t.Fatalf("seed account: %v", err)
	}
	acct.ID = aid
	if err := db.UpsertModels(ctx, aid, []provider.ModelInfo{{ID: "gpt-6.1-sol"}}); err != nil {
		t.Fatalf("seed catalog: %v", err)
	}

	tester := NewTester(client, provider.NewRegistry(client), db)
	tester.Run(ctx, acct, Options{Label: "copilot", APIKey: copilotRealKey})

	if tr.responses != 0 {
		t.Fatalf("a retired model must not be retried on another wire, got %d /responses calls", tr.responses)
	}
	rows, _ := db.ListModels(ctx, aid)
	for _, r := range rows {
		if r.Source == "probe" {
			t.Fatalf("a retired model must record no wire verdict, %s was probed", r.ID)
		}
	}
}

// retiredTransport: mint + catalog work, the chat endpoint reports a retired
// model (no wire named), and /responses counts any accidental call.
type retiredTransport struct{ responses int }

func (x *retiredTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	reply := func(status int, body string) (*http.Response, error) {
		h := make(http.Header)
		h.Set("Content-Type", "application/json")
		return &http.Response{StatusCode: status, Header: h,
			Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/copilot_internal/v2/token"):
		return reply(http.StatusOK, `{"token":"cop_tok","expires_at":`+itoa(time.Now().Add(time.Hour).Unix())+`}`)
	case strings.HasSuffix(r.URL.Path, "/models"):
		return reply(http.StatusOK, `{"data":[{"id":"gpt-6.1-sol"}]}`)
	case strings.HasSuffix(r.URL.Path, "/chat/completions"):
		return reply(http.StatusNotFound, `{"error":{"message":"the model has been retired and no longer available"}}`)
	case strings.HasSuffix(r.URL.Path, "/responses"):
		x.responses++
		return reply(http.StatusOK, `{"id":"r1","status":"completed","output":[]}`)
	}
	return reply(http.StatusNotFound, `{}`)
}
