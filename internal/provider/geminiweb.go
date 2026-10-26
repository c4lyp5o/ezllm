package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/c4lyp5o/ezllm/internal/geminiweb"
)

// KindGeminiWeb is Google Gemini's WEB protocol (gemini.google.com JSPB
// over wrb.fr frames) — see internal/geminiweb for the wire port. Accounts
// of this kind carry the sentinel base "internal://geminiweb"; the adapter
// rewrites requests to the in-process bridge (server/geminiweb_bridge.go),
// which owns cookie auth, the zero-quota status oracle and OpenAI-wire
// translation.
const KindGeminiWeb Kind = "gemini-web"

// Loopback headers stamped for the bridge. Both are stripped from inbound
// client requests (StripHeaders) so a client can never address an account's
// cookie material or impersonate another account row.
const (
	GeminiWebAccountHeader = "X-Ezllm-Gemini-Web-Account"
	GeminiWebKeyHeader     = "X-Ezllm-Gemini-Web-Key"
)

type geminiWebAdapter struct {
	client     *http.Client
	bridgeBase string // http://127.0.0.1:20129/internal/geminiweb/v1
	geminiBase string // https://gemini.google.com (tests inject a mock)
}

// NewGeminiWeb builds the gemini-web adapter. bridgeBase is the in-process
// bridge's base URL (derived from the server's own listen address).
func NewGeminiWeb(bridgeBase string, c *http.Client) Adapter {
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	return &geminiWebAdapter{client: c, bridgeBase: strings.TrimSuffix(bridgeBase, "/"), geminiBase: "https://gemini.google.com"}
}

// statusClient builds a geminiweb client for direct status/catalog calls.
func (a *geminiWebAdapter) statusClient(key string) (*geminiweb.Client, error) {
	c, err := geminiweb.NewFromKey(key)
	if err != nil {
		return nil, err
	}
	return c.WithBase(a.geminiBase), nil
}

func (a *geminiWebAdapter) Kind() Kind { return KindGeminiWeb }

func (a *geminiWebAdapter) StripHeaders() []string {
	return []string{GeminiWebAccountHeader, GeminiWebKeyHeader, BridgeSecretHeader}
}

// PrepareRequest routes the hop to the bridge: rewrites the sentinel base to
// the loopback URL, stamps the account row and the decrypted credential (the
// bridge prefers the stamped key so a bad-key calibration probe exercises
// the same auth path as live traffic), and drops any client-supplied
// Authorization. The bridge speaks plain OpenAI wire; nothing else applies.
func (a *geminiWebAdapter) PrepareRequest(_ context.Context, req *http.Request, acct Account, key string, _ Surface) error {
	if req.URL == nil {
		return fmt.Errorf("gemini-web: nil request URL")
	}
	path := req.URL.Path
	if raw := req.URL.RawQuery; raw != "" {
		path += "?" + raw
	}
	target, err := url.Parse(a.bridgeBase)
	if err != nil {
		return fmt.Errorf("gemini-web: bad bridge base %q: %w", a.bridgeBase, err)
	}
	// Keep the path the proxy resolved (/chat/completions, /models, …) and
	// re-root it on the bridge.
	target.Path = strings.TrimSuffix(target.Path, "/") + path
	req.URL = target
	req.Host = target.Host
	req.Header.Del("Authorization")
	req.Header.Del("X-Api-Key")
	req.Header.Set(GeminiWebAccountHeader, strconv.FormatInt(acct.ID, 10))
	req.Header.Set(BridgeSecretHeader, BridgeSecret())
	if key != "" {
		req.Header.Set(GeminiWebKeyHeader, key)
	}
	return nil
}

// AuthOracle: the bridge's GET /models runs Init + GET_USER_STATUS
// (zero-generation-quota) and answers 401 for dead cookies. The oracle
// Path is relative to BaseURL like every other kind.
func (a *geminiWebAdapter) AuthOracle() AuthProbe {
	return AuthProbe{Method: http.MethodGet, Path: "/models"}
}

func (a *geminiWebAdapter) CatalogIsPublic() bool { return false }

// ReadQuota reads the account's status as quota: the AccountStatus verdict
// plus the model count — the status RPC is the only report this protocol
// offers, and it costs no generation quota.
func (a *geminiWebAdapter) ReadQuota(ctx context.Context, _ Account, key string) (Quota, error) {
	c, err := a.statusClient(key)
	if err != nil {
		return Quota{}, err
	}
	if err := c.Init(ctx); err != nil {
		return Quota{}, err
	}
	st, err := c.GetUserStatus(ctx)
	if err != nil {
		return Quota{}, err
	}
	raw, err := json.Marshal(map[string]any{
		"status_code": st.Status.Code,
		"status_name": st.Status.Name,
		"models":      len(st.Models),
	})
	if err != nil {
		return Quota{}, err
	}
	return Quota{Kind: QuotaNone, Raw: string(raw), ReadAt: time.Now()}, nil
}

// ListModels syncs the catalog from the account cookie (zero quota). IDs are
// the derived names (gemini-<category>) so the router addresses models as
// "geminiweb/gemini-pro"; the raw hex stays in Raw for debugging.
func (a *geminiWebAdapter) ListModels(ctx context.Context, _ Account, key string) ([]ModelInfo, error) {
	c, err := a.statusClient(key)
	if err != nil {
		return nil, err
	}
	if err := c.Init(ctx); err != nil {
		return nil, err
	}
	st, err := c.GetUserStatus(ctx)
	if err != nil {
		return nil, err
	}
	if st.Status.Code != 1000 {
		return nil, fmt.Errorf("geminiweb: account status %d (%s)", st.Status.Code, st.Status.Name)
	}
	out := make([]ModelInfo, 0, len(st.Models))
	for _, m := range st.Models {
		id := m.Name
		if id == "" {
			id = m.ID // fall back to the raw model id
		}
		if id == "" {
			continue
		}
		display := m.DisplayName
		if display == "" {
			display = id
		}
		out = append(out, ModelInfo{
			ID:      id,
			OwnedBy: "google",
			Display: display + " (gemini-web)",
		})
	}
	return out, nil
}
