package geminiweb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// Endpoints (constants.py:Endpoint).
const (
	endpointInit      = "/app"
	endpointGenerate  = "/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate"
	endpointBatchExec = "/_/BardChatUi/data/batchexecute"

	// rotateURL is a field (not const) so tests can point it at a mock; the
	// production default is accounts.google.com/RotateCookies.
	rotateURL = "https://accounts.google.com/RotateCookies"
)

// Session tokens scraped from the Gemini web app's init page
// (get_access_token.py): the access token rides every request as form field
// `at`, the rest as query parameters.
type sessionTokens struct {
	accessToken string // SNlM0e
	buildLabel  string // cfb2h
	sessionID   string // FdrFJe
	language    string // TuX5cc
	pushID      string // qKIAYe (uploads only)
}

// Cookie holds the operator-exported credential pair.
type cookie struct {
	psid   string // __Secure-1PSID (long-lived)
	psidts string // __Secure-1PSIDTS (rotates ~10 min)
}

// Client speaks the Gemini web protocol for one account.
//
// Not safe for concurrent use of the mutable session fields (accessToken,
// cookies after rotation): one client per account, guarded by the caller.
type Client struct {
	base      string // https://gemini.google.com (injectable for tests)
	rotateURL string // https://accounts.google.com/RotateCookies (injectable for tests)
	http      *http.Client
	cookies   cookie
	tokens    sessionTokens
}

// ParseKey decodes the operator's stored credential into the cookie pair.
// Format: one cookie per line, either "name=value" or a bare value:
//
//	__Secure-1PSID=<value>
//	__Secure-1PSIDTS=<value>
//
// PSIDTS is optional (rotation re-fetches it). PSID is not.
func ParseKey(key string) (cookie, error) {
	var c cookie
	for _, line := range strings.Split(key, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, value := line, line
		if i := strings.IndexByte(line, '='); i >= 0 {
			name, value = line[:i], line[i+1:]
		}
		switch strings.TrimSpace(name) {
		case "__Secure-1PSID":
			c.psid = strings.TrimSpace(value)
		case "__Secure-1PSIDTS":
			c.psidts = strings.TrimSpace(value)
		}
	}
	if c.psid == "" {
		return cookie{}, errors.New("geminiweb: no __Secure-1PSID cookie found")
	}
	return c, nil
}

// Key serializes the cookie pair back to the stored-credential format
// (used by the rotator's persistence).
func (c cookie) Key() string {
	k := "__Secure-1PSID=" + c.psid
	if c.psidts != "" {
		k += "\n__Secure-1PSIDTS=" + c.psidts
	}
	return k
}

var (
	reAccessToken = regexp.MustCompile(`"SNlM0e":\s*"(.*?)"`)
	reBuildLabel  = regexp.MustCompile(`"cfb2h":\s*"(.*?)"`)
	reSessionID   = regexp.MustCompile(`"FdrFJe":\s*"(.*?)"`)
	reLanguage    = regexp.MustCompile(`"TuX5cc":\s*"(.*?)"`)
	rePushID      = regexp.MustCompile(`"qKIAYe":\s*"(.*?)"`)
)

// New builds a client for the given cookies. Call Init before use.
func New(c cookie) *Client {
	return &Client{base: "https://gemini.google.com", rotateURL: rotateURL, http: &http.Client{}, cookies: c}
}

// Init loads the app page and scrapes the session tokens. ErrNoAccessToken
// means the cookies did not authenticate (expired / wrong flavor) — the
// caller's key-test path maps it to a credential failure.
var ErrNoAccessToken = errors.New("geminiweb: init page yielded no SNlM0e token (cookies expired?)")

func (c *Client) Init(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+endpointInit, nil)
	if err != nil {
		return err
	}
	c.applyCookies(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	page := string(body)
	if m := reAccessToken.FindStringSubmatch(page); m != nil {
		c.tokens.accessToken = m[1]
	}
	if c.tokens.accessToken == "" {
		return ErrNoAccessToken
	}
	if m := reBuildLabel.FindStringSubmatch(page); m != nil {
		c.tokens.buildLabel = m[1]
	}
	if m := reSessionID.FindStringSubmatch(page); m != nil {
		c.tokens.sessionID = m[1]
	}
	if m := reLanguage.FindStringSubmatch(page); m != nil {
		c.tokens.language = m[1]
	}
	if m := rePushID.FindStringSubmatch(page); m != nil {
		c.tokens.pushID = m[1]
	}
	return nil
}

// applyCookies attaches the credential pair to a request.
func (c *Client) applyCookies(req *http.Request) {
	req.AddCookie(&http.Cookie{Name: "__Secure-1PSID", Value: c.cookies.psid})
	if c.cookies.psidts != "" {
		req.AddCookie(&http.Cookie{Name: "__Secure-1PSIDTS", Value: c.cookies.psidts})
	}
}

// Rotate refreshes __Secure-1PSIDTS (constants.py ROTATE_COOKIES, ~10 min
// cadence). On success the client's cookie is updated in place and the new
// credential key is returned for persistence; on failure the old cookies
// stay untouched and the caller retries next interval.
func (c *Client) Rotate(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.rotateURL, strings.NewReader("[]"))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://accounts.google.com")
	c.applyCookies(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("geminiweb: rotate cookies: status %d", resp.StatusCode)
	}
	var rotated string
	for _, ck := range resp.Cookies() {
		if ck.Name == "__Secure-1PSIDTS" && ck.Value != "" {
			rotated = ck.Value
		}
	}
	if rotated == "" {
		return "", errors.New("geminiweb: rotate cookies: no __Secure-1PSIDTS in response")
	}
	c.cookies.psidts = rotated
	return c.cookies.Key(), nil
}

// queryParams builds the standard query: hl, bl, f.sid (rt=c for generate).
func (c *Client) queryParams(rt string) url.Values {
	q := url.Values{}
	q.Set("hl", firstNonEmpty(c.tokens.language, "en"))
	if c.tokens.buildLabel != "" {
		q.Set("bl", c.tokens.buildLabel)
	}
	if c.tokens.sessionID != "" {
		q.Set("f.sid", c.tokens.sessionID)
	}
	if rt != "" {
		q.Set("rt", rt)
	}
	return q
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
