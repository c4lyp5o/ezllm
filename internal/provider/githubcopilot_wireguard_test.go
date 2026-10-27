package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Copilot serves claude* on /v1/messages and gemini* on /chat/completions, so a
// /responses dial answers 400 unsupported_api_for_model. The adapter must
// refuse BEFORE dialling and type the refusal as a wire fact the dispatcher can
// fail over on — a doomed request must never reach the wire.
func TestCopilotRefusesResponsesWireForClaudeAndGemini(t *testing.T) {
	for _, model := range []string{"claude-sonnet-5", "gemini-3.5-flash"} {
		var copilotHits int
		client := copilotRoutedClient(t, 200,
			`{"token":"tid-live","expires_at":1893456000}`,
			func(w http.ResponseWriter, r *http.Request) {
				copilotHits++
				w.WriteHeader(http.StatusOK)
			}, nil)
		a := newGitHubCopilotWithClient(client)

		req, _ := http.NewRequest(http.MethodPost, copilotAPIBase+"/responses",
			strings.NewReader(`{"model":"`+model+`","input":"hi"}`))
		req.Header.Set("Accept", "application/json")
		acct := Account{ID: 7, Kind: KindGitHubCopilot}

		err := a.PrepareRequest(context.Background(), req, acct, copilotSessionKey(t), SurfaceResponses)
		if !errors.Is(err, ErrWireUnsupported) {
			t.Fatalf("model %s: err = %v, want ErrWireUnsupported", model, err)
		}
		if !strings.Contains(err.Error(), model) {
			t.Errorf("model %s: refusal must name the model, got %v", model, err)
		}
		if copilotHits != 0 {
			t.Fatalf("model %s: %d request(s) reached api.githubcopilot.com — the guard must refuse before dialling",
				model, copilotHits)
		}
	}
}

// The guard is wire-scoped: a model that DOES speak the responses wire is
// untouched, and its body passes through byte-identical (no quirk pass on this
// wire — the quirks are /chat/completions-specific).
func TestCopilotAllowsResponsesWireForCodex(t *testing.T) {
	client := copilotRoutedClient(t, 200, `{"token":"tid-live","expires_at":1893456000}`,
		func(w http.ResponseWriter, r *http.Request) { t.Fatalf("no upstream call expected") }, nil)
	a := newGitHubCopilotWithClient(client)

	const body = `{"model":"gpt-5.3-codex","input":"hi"}`
	req, _ := http.NewRequest(http.MethodPost, copilotAPIBase+"/responses", strings.NewReader(body))
	req.Header.Set("Accept", "application/json")
	acct := Account{ID: 7, Kind: KindGitHubCopilot}

	if err := a.PrepareRequest(context.Background(), req, acct, copilotSessionKey(t), SurfaceResponses); err != nil {
		t.Fatalf("codex on /responses must be allowed, got %v", err)
	}
	got, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Errorf("body = %s, want byte-identical %s", got, body)
	}
}

// The stealth rule reaches the wire on the chat surface: the persona is gone
// before the request leaves the adapter (a neutral system turn replaces it).
func TestCopilotPrepareRequestDropsSystemMessages(t *testing.T) {
	client := copilotRoutedClient(t, 200, `{"token":"tid-live","expires_at":1893456000}`,
		func(w http.ResponseWriter, r *http.Request) { t.Fatalf("no upstream call expected") }, nil)
	a := newGitHubCopilotWithClient(client)

	req, _ := http.NewRequest(http.MethodPost, copilotAPIBase+"/chat/completions",
		strings.NewReader(`{"model":"gpt-5.5","messages":[{"role":"system","content":"SOUL.md"},{"role":"user","content":"hi"}]}`))
	req.Header.Set("Accept", "application/json")
	acct := Account{ID: 7, Kind: KindGitHubCopilot}

	if err := a.PrepareRequest(context.Background(), req, acct, copilotSessionKey(t), SurfaceChat); err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(req.Body)
	if strings.Contains(string(got), "SOUL.md") {
		t.Errorf("persona reached the outbound body: %s", got)
	}
	if !strings.Contains(string(got), `"hi"`) {
		t.Errorf("user turn lost: %s", got)
	}
}

// The responses wire runs no quirk pass, but the privacy rule still reaches it:
// instructions carry the client's persona on this wire.
func TestCopilotPrepareRequestStripsPersonaOnResponses(t *testing.T) {
	client := copilotRoutedClient(t, 200, `{"token":"tid-live","expires_at":1893456000}`,
		func(w http.ResponseWriter, r *http.Request) { t.Fatalf("no upstream call expected") }, nil)
	a := newGitHubCopilotWithClient(client)

	req, _ := http.NewRequest(http.MethodPost, copilotAPIBase+"/responses",
		strings.NewReader(`{"model":"gpt-5.3-codex","instructions":"SOUL.md","input":"hi"}`))
	req.Header.Set("Accept", "application/json")
	acct := Account{ID: 7, Kind: KindGitHubCopilot}

	if err := a.PrepareRequest(context.Background(), req, acct, copilotSessionKey(t), SurfaceResponses); err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(req.Body)
	if strings.Contains(string(got), "SOUL.md") {
		t.Errorf("persona reached the outbound body: %s", got)
	}
	if req.ContentLength != int64(len(got)) {
		t.Errorf("ContentLength = %d, body is %d bytes", req.ContentLength, len(got))
	}
}
