package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/githubcopilot"
)

// The adapter's tests route GitHub's + Copilot's endpoints through an
// injected client, same technique as internal/githubcopilot's own tests.

func copilotRoutedClient(t *testing.T, mintStatus int, mintBody string, copilotHandler http.HandlerFunc, mints *int) *http.Client {
	t.Helper()
	return &http.Client{Transport: roundTripFuncCopilot(func(req *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		switch req.URL.Host {
		case "api.github.com":
			if mints != nil {
				*mints++
			}
			rec.Header().Set("Content-Type", "application/json")
			rec.WriteHeader(mintStatus)
			fmt.Fprint(rec, mintBody)
		case "api.githubcopilot.com":
			copilotHandler(rec, req)
		default:
			rec.WriteHeader(404)
		}
		return rec.Result(), nil
	})}
}

type roundTripFuncCopilot func(*http.Request) (*http.Response, error)

func (f roundTripFuncCopilot) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func copilotSessionKey(t *testing.T) string {
	t.Helper()
	return githubcopilot.Session{AccessToken: "gho_test", RefreshToken: "ghr_test"}.Encode()
}

func TestCopilotAdapterShape(t *testing.T) {
	a := NewGitHubCopilot(nil)
	if a.Kind() != KindGitHubCopilot {
		t.Fatalf("kind = %s", a.Kind())
	}
	if a.CatalogIsPublic() {
		t.Error("catalog must not be public (a dead token must 401)")
	}
	oracle := a.AuthOracle()
	if oracle.Method != http.MethodGet || oracle.Path != "/models" {
		t.Errorf("oracle = %+v", oracle)
	}
	if _, err := a.ReadQuota(context.Background(), Account{}, "key"); err != ErrQuotaUnsupported {
		t.Errorf("quota err = %v, want ErrQuotaUnsupported (no invented numbers)", err)
	}
	strip := a.StripHeaders()
	for _, want := range []string{"Copilot-Integration-Id", "X-Initiator"} {
		found := false
		for _, s := range strip {
			if s == want {
				found = true
			}
		}
		if !found {
			t.Errorf("StripHeaders missing %q", want)
		}
	}
}

func TestCopilotPrepareRequestMintsAndHeaders(t *testing.T) {
	mints := 0
	copilotH := func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
	}
	client := copilotRoutedClient(t, 200,
		`{"token":"tid-live","expires_at":1893456000}`, copilotH, &mints)
	a := newGitHubCopilotWithClient(client)

	req, _ := http.NewRequest(http.MethodPost, copilotAPIBase+"/chat/completions",
		strings.NewReader(`{"model":"gpt-5.4","temperature":0.5,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Initiator", "agent") // client-supplied: must be replaced
	acct := Account{ID: 7, Kind: KindGitHubCopilot}
	if err := a.PrepareRequest(context.Background(), req, acct, copilotSessionKey(t), SurfaceChat); err != nil {
		t.Fatal(err)
	}
	if mints != 1 {
		t.Fatalf("mints = %d, want 1", mints)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer tid-live" {
		t.Errorf("auth = %q", got)
	}
	if req.Header.Get("X-Initiator") != "user" {
		t.Errorf("X-Initiator = %q, want user (client value must not pin billing)", req.Header.Get("X-Initiator"))
	}
	if req.Header.Get("copilot-harness-id") != "copilot-sdk" {
		t.Errorf("harness = %q", req.Header.Get("copilot-harness-id"))
	}
	// The gpt-5.4 quirk must have been applied to the forwarded body.
	body, _ := io.ReadAll(req.Body)
	if strings.Contains(string(body), "temperature") {
		t.Errorf("temperature survived: %s", body)
	}

	// Second call: the cached session means no second mint.
	req2, _ := http.NewRequest(http.MethodPost, copilotAPIBase+"/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	if err := a.PrepareRequest(context.Background(), req2, acct, copilotSessionKey(t), SurfaceChat); err != nil {
		t.Fatal(err)
	}
	if mints != 1 {
		t.Errorf("mints = %d, want the cached token reuse", mints)
	}
}

func TestCopilotPrepareRequestMessagesAnchor(t *testing.T) {
	client := copilotRoutedClient(t, 200, `{"token":"tid","expires_at":1893456000}`,
		func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "{}") }, nil)
	a := newGitHubCopilotWithClient(client)
	req, _ := http.NewRequest(http.MethodPost, copilotAPIBase+"/messages",
		strings.NewReader(`{"model":"claude-opus-4.6","messages":[{"role":"user","content":"x"}]}`))
	req.Header.Set("Accept", "application/json")
	if err := a.PrepareRequest(context.Background(), req, Account{ID: 1}, copilotSessionKey(t), SurfaceAnthropic); err != nil {
		t.Fatal(err)
	}
	if req.URL.Path != "/v1/messages" {
		t.Errorf("path = %s, want /v1/messages (Copilot's shim is version-anchored)", req.URL.Path)
	}
}

func TestCopilotPrepareRequestDeadCredential(t *testing.T) {
	client := copilotRoutedClient(t, 403, `{"message":"Bad credentials"}`,
		func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "{}") }, nil)
	a := newGitHubCopilotWithClient(client)
	req, _ := http.NewRequest(http.MethodPost, copilotAPIBase+"/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	err := a.PrepareRequest(context.Background(), req, Account{ID: 1}, copilotSessionKey(t), SurfaceChat)
	if err == nil {
		t.Fatal("403 on mint must fail PrepareRequest (this is the calibration path)")
	}
}

func TestCopilotListModels(t *testing.T) {
	payload := `{"data":[{"id":"gpt-4o","name":"GPT-4o","model_picker_enabled":true,"policy":{"state":"enabled"},"capabilities":{"type":"chat"}}]}`
	client := copilotRoutedClient(t, 200, `{"token":"tid","expires_at":1893456000}`,
		func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/models" {
				w.WriteHeader(404)
				return
			}
			if r.Header.Get("Authorization") != "Bearer tid" {
				t.Errorf("catalog auth = %q", r.Header.Get("Authorization"))
			}
			fmt.Fprint(w, payload)
		}, nil)
	a := newGitHubCopilotWithClient(client)
	models, err := a.ListModels(context.Background(), Account{ID: 3}, copilotSessionKey(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "gpt-4o" || models[0].OwnedBy != "github" {
		t.Fatalf("models = %+v", models)
	}
}

func TestCopilotSessionCacheIsolatesAccounts(t *testing.T) {
	mints := 0
	client := copilotRoutedClient(t, 200, `{"token":"tid","expires_at":1893456000}`,
		func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"data":[]}`) }, &mints)
	a := newGitHubCopilotWithClient(client)
	key := copilotSessionKey(t)
	for _, id := range []int64{1, 2} {
		req, _ := http.NewRequest(http.MethodPost, copilotAPIBase+"/chat/completions",
			strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
		if err := a.PrepareRequest(context.Background(), req, Account{ID: id}, key, SurfaceChat); err != nil {
			t.Fatal(err)
		}
	}
	if mints != 2 {
		t.Errorf("mints = %d, want one per account (no cross-account token sharing)", mints)
	}
}
