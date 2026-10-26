package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/c4lyp5o/ezllm/internal/githubcopilot"
)

// KindGitHubCopilot is GitHub Copilot through the official CLI's public
// client id + device flow (see internal/githubcopilot and
// docs/github-copilot-design.md). Unlike gemini-web, Copilot speaks
// OpenAI/Anthropic/Responses wires, so no bridge is needed — the adapter
// owns auth lifecycle, endpoint anchoring and body quirks in place.
const KindGitHubCopilot Kind = "github-copilot"

const (
	copilotCLIEnv    = "GITHUB_COPILOT_CLI_VERSION"
	copilotClientEnv = "GITHUB_OAUTH_CLIENT_ID"
	copilotAPIBase   = "https://api.githubcopilot.com"
)

type gitHubCopilotAdapter struct {
	client *http.Client

	mu       sync.Mutex
	sessions map[int64]githubcopilot.Session // ensured sessions per account
	cfg      githubcopilot.Config
}

// NewGitHubCopilot builds the adapter. The CLI version and (optionally) the
// operator's own client id come from the environment — the 466 floor
// recurs whenever GitHub raises it.
func NewGitHubCopilot(c *http.Client) Adapter {
	if c == nil {
		c = &http.Client{Timeout: 60 * time.Second}
	}
	return &gitHubCopilotAdapter{
		client:   c,
		sessions: map[int64]githubcopilot.Session{},
		cfg: githubcopilot.Config{
			HTTP:       c,
			CLIVersion: envOr(copilotCLIEnv, ""),
			ClientID:   envOr(copilotClientEnv, ""),
		},
	}
}

// newGitHubCopilotWithClient is the test constructor: the routed HTTP client
// stands in for GitHub's endpoints.
func newGitHubCopilotWithClient(c *http.Client) Adapter {
	return &gitHubCopilotAdapter{
		client:   c,
		sessions: map[int64]githubcopilot.Session{},
		cfg:      githubcopilot.Config{HTTP: c},
	}
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func (a *gitHubCopilotAdapter) Kind() Kind { return KindGitHubCopilot }

// StripHeaders: a client must not be able to pin Copilot's correlation
// headers (they decide billing and fingerprinting upstream).
func (a *gitHubCopilotAdapter) StripHeaders() []string {
	return []string{
		"Copilot-Integration-Id", "Editor-Version", "Editor-Plugin-Version",
		"OpenAI-Intent", "X-Interaction-Type", "Copilot-Harness-Id",
		"X-Github-Api-Version", "X-Initiator", "X-Client-Machine-Id",
	}
}

// PrepareRequest mints/refreshes the Copilot token (cached per account),
// installs the CLI header set, anchors the Anthropic shim at /v1/messages,
// and applies the body quirks for the requested model.
func (a *gitHubCopilotAdapter) PrepareRequest(ctx context.Context, req *http.Request, acct Account, key string, surface Surface) error {
	if req.URL == nil {
		return fmt.Errorf("github-copilot: nil request URL")
	}
	session, err := githubcopilot.DecodeSession(key)
	if err != nil {
		// Decided locally with no network involved: a blob the adapter cannot
		// decode is a credential it cannot use.
		return fmt.Errorf("%w: %v", ErrCredentialRefused, err)
	}
	ensured, err := a.ensureSession(ctx, acct.ID, session)
	if err != nil {
		return credentialErr(err)
	}

	// Endpoint anchoring: Copilot's Anthropic shim lives at /v1/messages
	// (SurfacePath yields /messages). Query strings are dropped — the
	// inference endpoints take none.
	if p := req.URL.Path; p == "/messages" {
		req.URL.Path = "/v1/messages"
	}
	req.URL.RawQuery = ""

	// Read the body ONCE: the model id selects the quirks and the patched
	// body goes back on the request.
	var (
		raw     []byte
		bodyObj map[string]any
		model   string
	)
	if req.Body != nil && (surface == SurfaceChat || surface == SurfaceAnthropic) {
		raw, err = io.ReadAll(io.LimitReader(req.Body, 32<<20))
		if err != nil {
			return fmt.Errorf("github-copilot: read body: %w", err)
		}
		_ = req.Body.Close()
		if err := json.Unmarshal(raw, &bodyObj); err == nil {
			model, _ = bodyObj["model"].(string)
			githubcopilot.PrepareBody(model, bodyObj)
			if patched, err := json.Marshal(bodyObj); err == nil {
				raw = patched
			}
		}
		req.Body = io.NopCloser(strings.NewReader(string(raw)))
		req.ContentLength = int64(len(raw))
	}

	// Headers: the full CLI set (streaming marker on SSE).
	stream := req.Header.Get("Accept") == "text/event-stream"
	h := githubcopilot.ChatHeaders(a.cfg, ensured.CopilotToken, stream)
	for k := range h {
		req.Header.Del(k)
	}
	for k, vs := range h {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	return nil
}

// ensureSession caches the minted token per account and re-mints on the
// 5-minute margin. In-memory only: a rotated GitHub refresh token is not
// persisted by the adapter (documented in the runbook — the device-flow
// CLI re-authorizes when GitHub invalidates it).
func (a *gitHubCopilotAdapter) ensureSession(ctx context.Context, acctID int64, fromKey githubcopilot.Session) (githubcopilot.Session, error) {
	a.mu.Lock()
	cached, ok := a.sessions[acctID]
	a.mu.Unlock()
	if ok && !githubcopilot.NeedsRefresh(cached, time.Now()) &&
		cached.AccessToken == fromKey.AccessToken {
		return cached, nil
	}
	ensured, err := githubcopilot.EnsureSession(ctx, a.cfg, fromKey, time.Now())
	if err != nil {
		return githubcopilot.Session{}, err
	}
	a.mu.Lock()
	a.sessions[acctID] = ensured
	a.mu.Unlock()
	return ensured, nil
}

// AuthOracle: GET /models through the normal PrepareRequest path (the mint
// happens there), so a dead credential fails exactly like live traffic.
func (a *gitHubCopilotAdapter) AuthOracle() AuthProbe {
	return AuthProbe{Method: http.MethodGet, Path: "/models"}
}

func (a *gitHubCopilotAdapter) CatalogIsPublic() bool { return false }

// credentialErr marks a provider-side REFUSAL of the credential exchange.
// Only a 401/403 from the mint becomes ErrCredentialRefused: a transport
// failure (unreachable host, timeout) or a 5xx stays an ordinary error, so an
// outage can never be mistaken for a proven credential by the key test.
func credentialErr(err error) error {
	var me *githubcopilot.MintError
	if errors.As(err, &me) && (me.Status == http.StatusUnauthorized || me.Status == http.StatusForbidden) {
		return fmt.Errorf("%w: %v", ErrCredentialRefused, err)
	}
	return err
}

// ReadQuota: Copilot exposes no quota endpoint on this surface — report
// unsupported rather than inventing numbers.
func (a *gitHubCopilotAdapter) ReadQuota(context.Context, Account, string) (Quota, error) {
	return Quota{}, ErrQuotaUnsupported
}

// ListModels syncs the account's catalog through the minted token.
func (a *gitHubCopilotAdapter) ListModels(ctx context.Context, acct Account, key string) ([]ModelInfo, error) {
	session, err := githubcopilot.DecodeSession(key)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCredentialRefused, err)
	}
	ensured, err := a.ensureSession(ctx, acct.ID, session)
	if err != nil {
		return nil, credentialErr(err)
	}
	models, err := githubcopilot.ListModels(ctx, a.cfg, ensured.CopilotToken)
	if err != nil {
		return nil, err
	}
	out := make([]ModelInfo, 0, len(models))
	for _, m := range models {
		display := m.Name
		if display == "" {
			display = m.ID
		}
		out = append(out, ModelInfo{ID: m.ID, OwnedBy: "github", Display: display})
	}
	return out, nil
}
