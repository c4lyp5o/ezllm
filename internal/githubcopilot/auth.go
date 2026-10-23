// Package githubcopilot implements GitHub Copilot as a provider for ezllm,
// ported from omniroute's production implementation (verified working
// 2026-10-02; design: docs/github-copilot-design.md, evidence:
// docs/github-copilot-policy-evidence.md).
//
// The auth path is the OFFICIAL Copilot CLI's: device flow (RFC 8628) with
// GitHub's public Copilot CLI client id and scope read:user, then minting a
// short-lived Copilot session token from api.github.com. No custom OAuth app
// (those are restricted-tier / 403), no client-id spoofing.
package githubcopilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// OAuth endpoints (omniroute constants, verified live 2026-10-02).
const (
	defaultClientID   = "Iv1.b507a08c87ecfe98" // public Copilot CLI client id
	deviceCodeURL     = "https://github.com/login/device/code"
	accessTokenURL    = "https://github.com/login/oauth/access_token"
	userInfoURL       = "https://api.github.com/user"
	copilotTokenURL   = "https://api.github.com/copilot_internal/v2/token"
	copilotAPIBase    = "https://api.githubcopilot.com"
	apiVersion        = "2026-08-01"
	defaultScope      = "read:user"
	defaultCLIVersion = "1.0.103"
)

// Session is the stored credential row (provider_keys). It carries the
// GitHub OAuth pair plus the minted Copilot token; everything is
// account-live material — 0600 encrypted storage, never logged.
type Session struct {
	AccessToken      string    `json:"access_token"`
	RefreshToken     string    `json:"refresh_token,omitempty"`
	CopilotToken     string    `json:"copilot_token,omitempty"`
	CopilotExpiresAt time.Time `json:"copilot_expires_at,omitempty"`
	GitHubLogin      string    `json:"github_login,omitempty"`
}

// Encode serializes a session for the key store.
func (s Session) Encode() string {
	b, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	return string(b)
}

// DecodeSession parses a stored credential row. Legacy rows holding a raw
// GITHUB token (a bare "ghu_…"/"gho_…" string) are lifted into a session.
func DecodeSession(raw string) (Session, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Session{}, errors.New("githubcopilot: empty credential")
	}
	if strings.HasPrefix(raw, "{") {
		var s Session
		if err := json.Unmarshal([]byte(raw), &s); err != nil {
			return Session{}, fmt.Errorf("githubcopilot: decode session: %w", err)
		}
		if s.AccessToken == "" {
			return Session{}, errors.New("githubcopilot: session has no access token")
		}
		return s, nil
	}
	// Legacy: a bare OAuth token string.
	return Session{AccessToken: raw}, nil
}

// Config overrides (env-driven, see the runbook): operator client id for
// those who registered their own app, and the CLI version floor.
type Config struct {
	ClientID    string
	CLIVersion  string
	Scope       string
	Integration string // Copilot-Integration-Id; default copilot-developer-cli
	HTTP        *http.Client
	// Sleep is the poll wait; tests inject an instant one (no test sleeps).
	Sleep func(context.Context, time.Duration) error
}

func (c Config) withDefaults() Config {
	if c.ClientID == "" {
		c.ClientID = defaultClientID
	}
	if c.CLIVersion == "" {
		c.CLIVersion = defaultCLIVersion
	}
	if c.Scope == "" {
		c.Scope = defaultScope
	}
	if c.Integration == "" {
		c.Integration = "copilot-developer-cli"
	}
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	if c.Sleep == nil {
		c.Sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		}
	}
	return c
}

// DeviceCode is one step of the interactive flow.
type DeviceCode struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// RequestDeviceCode starts the device flow. The operator visits
// VerificationURI and enters UserCode; PollAccessToken completes it.
func RequestDeviceCode(ctx context.Context, cfg Config) (DeviceCode, error) {
	cfg = cfg.withDefaults()
	form := url.Values{"client_id": {cfg.ClientID}, "scope": {cfg.Scope}}
	resp, err := postForm(ctx, cfg.HTTP, deviceCodeURL, form, nil)
	if err != nil {
		return DeviceCode{}, err
	}
	var dc DeviceCode
	if err := json.Unmarshal(resp, &dc); err != nil {
		return DeviceCode{}, fmt.Errorf("githubcopilot: device code: %w", err)
	}
	if dc.DeviceCode == "" || dc.UserCode == "" {
		return DeviceCode{}, fmt.Errorf("githubcopilot: device code response missing fields: %s", resp)
	}
	if dc.Interval == 0 {
		dc.Interval = 5
	}
	if dc.VerificationURI == "" {
		dc.VerificationURI = "https://github.com/login/device"
	}
	return dc, nil
}

// ErrAuthorizationPending is the poll's "keep waiting" signal (GitHub's
// slow_down/authorization_pending).
var ErrAuthorizationPending = errors.New("githubcopilot: authorization pending")

// pollDeviceOnce performs ONE RFC 8628 token poll. The returned string is
// the RFC's pending signal: "" (with a session) means the grant is in,
// "authorization_pending"/"slow_down" means keep polling on the interval,
// anything else is fatal. Split out of PollAccessToken so the dashboard's
// device flow can drive its own loop — one HTTP round trip per GET instead
// of a handler held open for the 15 minutes a human needs to tap Authorize.
func pollDeviceOnce(ctx context.Context, cfg Config, dc DeviceCode) (Session, string, error) {
	cfg = cfg.withDefaults()
	form := url.Values{
		"client_id":   {cfg.ClientID},
		"device_code": {dc.DeviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	}
	body, err := postForm(ctx, cfg.HTTP, accessTokenURL, form, nil)
	if err != nil {
		return Session{}, "", err
	}
	var tr struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		TokenType    string `json:"token_type"`
		Error        string `json:"error"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return Session{}, "", fmt.Errorf("githubcopilot: token poll: %w", err)
	}
	switch tr.Error {
	case "authorization_pending", "slow_down":
		return Session{}, tr.Error, nil
	case "":
		if tr.AccessToken == "" {
			// No token and no error: GitHub's documented "keep polling".
			return Session{}, "authorization_pending", nil
		}
		return Session{AccessToken: tr.AccessToken, RefreshToken: tr.RefreshToken}, "", nil
	default:
		return Session{}, "", fmt.Errorf("githubcopilot: device flow %s", tr.Error)
	}
}

// PollState is one RFC 8628 poll classified for a caller that owns its own
// schedule. The dashboard needs slow_down SEPARATELY from authorization_pending:
// GitHub will not hand over the token until the poller lengthens its interval
// (RFC 8628 §3.5), so a poller that cannot tell them apart stalls forever while
// github.com/login/device already shows "Success".
type PollState int

const (
	PollPending  PollState = iota // authorization_pending, or an empty response
	PollSlowDown                  // GitHub asked for a LONGER interval
	PollGranted                   // session returned
)

// PollDeviceState is one poll with the pending reason preserved. ok=false (with
// err==nil) means keep polling; err means GitHub or the network refused.
func PollDeviceState(ctx context.Context, cfg Config, dc DeviceCode) (Session, PollState, error) {
	s, pending, err := pollDeviceOnce(ctx, cfg, dc)
	if err != nil {
		return Session{}, PollPending, err
	}
	switch pending {
	case "":
		return s, PollGranted, nil
	case "slow_down":
		return Session{}, PollSlowDown, nil
	default:
		return Session{}, PollPending, nil
	}
}

// PollAccessTokenOnce is one poll for callers that own their own schedule —
// the dashboard device flow asks once per status GET rather than blocking.
// ok=false means the operator has not authorized yet; ask again.
// (slow_down is folded into ok=false here — use PollDeviceState to honour it.)
func PollAccessTokenOnce(ctx context.Context, cfg Config, dc DeviceCode) (Session, bool, error) {
	s, state, err := PollDeviceState(ctx, cfg, dc)
	if err != nil {
		return Session{}, false, err
	}
	if state != PollGranted {
		return Session{}, false, nil
	}
	return s, true, nil
}

// PollAccessToken polls until the operator authorizes (or the code
// expires / is denied). ctx cancellation aborts the wait.
func PollAccessToken(ctx context.Context, cfg Config, dc DeviceCode) (Session, error) {
	cfg = cfg.withDefaults()
	interval := time.Duration(dc.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	deadline := time.Now().Add(time.Duration(dc.ExpiresIn) * time.Second)
	if dc.ExpiresIn == 0 {
		deadline = time.Now().Add(15 * time.Minute)
	}
	for {
		if err := cfg.Sleep(ctx, interval); err != nil {
			return Session{}, err
		}
		if time.Now().After(deadline) {
			return Session{}, errors.New("githubcopilot: device code expired")
		}
		s, pending, err := pollDeviceOnce(ctx, cfg, dc)
		if err != nil {
			return Session{}, err
		}
		switch pending {
		case "":
			return s, nil
		case "slow_down":
			// GitHub asks for +5s; honor it.
			interval += 5 * time.Second
		}
	}
}

// MintCopilotToken exchanges a GitHub token for a short-lived Copilot
// session token. The header set is the one the official CLI uses.
func MintCopilotToken(ctx context.Context, cfg Config, githubToken string) (string, time.Time, error) {
	cfg = cfg.withDefaults()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, copilotTokenURL, nil)
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Authorization", "token "+githubToken) // 'token', not Bearer
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", "GitHubCopilotChat/"+cfg.CLIVersion)
	req.Header.Set("Editor-Version", "copilot/"+cfg.CLIVersion)
	req.Header.Set("Editor-Plugin-Version", "copilot-chat/"+cfg.CLIVersion)
	resp, err := cfg.HTTP.Do(req)
	if err != nil {
		return "", time.Time{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("githubcopilot: copilot token mint: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", time.Time{}, fmt.Errorf("githubcopilot: copilot token mint: %w", err)
	}
	if out.Token == "" {
		return "", time.Time{}, errors.New("githubcopilot: copilot token mint: empty token")
	}
	return out.Token, time.Unix(out.ExpiresAt, 0).UTC(), nil
}

// RefreshGitHubToken performs the refresh_token grant with the public
// client id — NO client_secret (the CLI is a public client; sending
// "undefined" was 9router#442).
func RefreshGitHubToken(ctx context.Context, cfg Config, refreshToken string) (Session, error) {
	cfg = cfg.withDefaults()
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {cfg.ClientID},
	}
	body, err := postForm(ctx, cfg.HTTP, accessTokenURL, form, nil)
	if err != nil {
		return Session{}, err
	}
	var tr struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Error        string `json:"error"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return Session{}, fmt.Errorf("githubcopilot: refresh: %w", err)
	}
	if tr.Error != "" || tr.AccessToken == "" {
		return Session{}, fmt.Errorf("githubcopilot: refresh failed: %s", firstNonEmpty(tr.Error, "empty response"))
	}
	s := Session{AccessToken: tr.AccessToken}
	if tr.RefreshToken != "" {
		s.RefreshToken = tr.RefreshToken
	} else {
		s.RefreshToken = refreshToken
	}
	return s, nil
}

// UserInfo is the cheapest identity read (used by the device-flow CLI).
type UserInfo struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

func FetchUserInfo(ctx context.Context, cfg Config, githubToken string) (UserInfo, error) {
	cfg = cfg.withDefaults()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, userInfoURL, nil)
	if err != nil {
		return UserInfo{}, err
	}
	setGitHubHeaders(req, cfg.CLIVersion)
	req.Header.Set("Authorization", "Bearer "+githubToken)
	resp, err := cfg.HTTP.Do(req)
	if err != nil {
		return UserInfo{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return UserInfo{}, fmt.Errorf("githubcopilot: user info: status %d", resp.StatusCode)
	}
	var u UserInfo
	if err := json.Unmarshal(body, &u); err != nil {
		return UserInfo{}, fmt.Errorf("githubcopilot: user info: %w", err)
	}
	return u, nil
}

func postForm(ctx context.Context, c *http.Client, endpoint string, form url.Values, headers map[string]string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("githubcopilot: %s: status %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

func setGitHubHeaders(req *http.Request, cliVersion string) {
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", "GitHubCopilotChat/"+cliVersion)
	req.Header.Set("Editor-Version", "copilot/"+cliVersion)
	req.Header.Set("Editor-Plugin-Version", "copilot-chat/"+cliVersion)
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
