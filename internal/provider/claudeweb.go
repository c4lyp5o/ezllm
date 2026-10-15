package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// KindClaudeWeb is Claude.ai's WEB protocol (sessionKey cookie, SSE
// completion) — see internal/claudeweb and docs/claude-web-design.md.
// Accounts carry the sentinel base "internal://claudeweb"; the adapter
// rewrites requests to the in-process bridge, which owns the
// TLS-fingerprinted transport, org discovery and SSE parsing.
const KindClaudeWeb Kind = "claude-web"

// Loopback headers stamped for the bridge (same contract as gemini-web).
const (
	ClaudeWebAccountHeader = "X-Ezllm-Claude-Web-Account"
	ClaudeWebKeyHeader     = "X-Ezllm-Claude-Web-Key"
)

// ClaudeWebDefaultModels is the BOOTSTRAP catalog for this kind. The web
// protocol has NO model discovery — the account's entitlement is revealed by
// the first upstream answer, not by a catalog call. "claude-opus-5" and
// "claude-sonnet-4-6" are the ids the reference implementation pins; the
// rest follow claude-web's hyphenated-version convention (NOT the dotted
// Copilot-surface ids — different protocol) and are unverified until an
// upstream answer proves them. The bridge passes ANY id through; this set
// only seeds routing so the router's fail-closed model check has rows.
var ClaudeWebDefaultModels = []string{
	"claude-opus-5",
	"claude-sonnet-4-6",
	"claude-opus-4-6",
	"claude-sonnet-4-5",
	"claude-haiku-4-5",
}

type claudeWebAdapter struct {
	client     *http.Client
	bridgeBase string // http://127.0.0.1:20129/internal/claudeweb/v1
}

// NewClaudeWeb builds the claude-web adapter. bridgeBase is the in-process
// bridge's base URL (from the server's listen address).
func NewClaudeWeb(bridgeBase string, c *http.Client) Adapter {
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	return &claudeWebAdapter{client: c, bridgeBase: strings.TrimSuffix(bridgeBase, "/")}
}

// newClaudeWebAdapterForTest is the test constructor: bridgeBase points the
// rewrite at a mock.
func newClaudeWebAdapterForTest(bridgeBase string) Adapter {
	return &claudeWebAdapter{client: &http.Client{Timeout: 5 * time.Second}, bridgeBase: strings.TrimSuffix(bridgeBase, "/")}
}

func (a *claudeWebAdapter) Kind() Kind { return KindClaudeWeb }

func (a *claudeWebAdapter) StripHeaders() []string {
	return []string{ClaudeWebAccountHeader, ClaudeWebKeyHeader}
}

// PrepareRequest routes the hop to the bridge: rewrites the sentinel base to
// the loopback URL and stamps the account id + decrypted credential.
func (a *claudeWebAdapter) PrepareRequest(_ context.Context, req *http.Request, acct Account, key string, _ Surface) error {
	if req.URL == nil {
		return fmt.Errorf("claude-web: nil request URL")
	}
	path := req.URL.Path
	if raw := req.URL.RawQuery; raw != "" {
		path += "?" + raw
	}
	target, err := url.Parse(a.bridgeBase)
	if err != nil {
		return fmt.Errorf("claude-web: bad bridge base %q: %w", a.bridgeBase, err)
	}
	target.Path = strings.TrimSuffix(target.Path, "/") + path
	req.URL = target
	req.Host = target.Host
	req.Header.Del("Authorization")
	req.Header.Del("X-Api-Key")
	req.Header.Set(ClaudeWebAccountHeader, strconv.FormatInt(acct.ID, 10))
	if key != "" {
		req.Header.Set(ClaudeWebKeyHeader, key)
	}
	return nil
}

// AuthOracle: GET /models on the bridge — the bridge's models route first
// runs org discovery (the zero-quota credential proof) before answering.
func (a *claudeWebAdapter) AuthOracle() AuthProbe {
	return AuthProbe{Method: http.MethodGet, Path: "/models"}
}

func (a *claudeWebAdapter) CatalogIsPublic() bool { return false }

// ReadQuota: the web protocol exposes no quota endpoint — unsupported, not
// invented.
func (a *claudeWebAdapter) ReadQuota(context.Context, Account, string) (Quota, error) {
	return Quota{}, ErrQuotaUnsupported
}

// ListModels is the bootstrap catalog: no discovery exists for this kind, so
// the documented default set is returned (never entitlement truth). The
// first upstream answer reveals what the account can actually serve.
func (a *claudeWebAdapter) ListModels(context.Context, Account, string) ([]ModelInfo, error) {
	out := make([]ModelInfo, 0, len(ClaudeWebDefaultModels))
	for _, id := range ClaudeWebDefaultModels {
		out = append(out, ModelInfo{ID: id, OwnedBy: "anthropic", Display: id + " (claude-web)"})
	}
	return out, nil
}
