package claudeweb

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Failure modes — typed, matching omniroute's OrganizationResolution.
var (
	ErrSessionExpired      = errors.New("claudeweb: session expired or invalid")
	ErrCloudflareChallenge = errors.New("claudeweb: cloudflare browser challenge")
	ErrForbidden           = errors.New("claudeweb: forbidden")
	ErrUnavailable         = errors.New("claudeweb: upstream unavailable")
	ErrNoOrganization      = errors.New("claudeweb: no organization resolvable for this session")
	ErrRateLimited         = errors.New("claudeweb: rate limited")
	ErrStreamCut           = errors.New("claudeweb: stream ended before message_stop")
)

const sessionCookieName = "sessionKey"

// NormalizeSessionCookie validates an operator-supplied credential and
// returns the Cookie header value. It accepts:
//
//   - a bare sessionKey value (sk-ant-sid02-…)
//   - sessionKey=sk-ant-…
//   - the full claude.ai cookie header (sessionKey=…; cf_clearance=…; …)
//
// The full header is what the live wire actually requires: Claude/Cloudflare
// answers a sessionKey-only request with a synthetic
// 429 {"type":"rate_limit_error"} in ~30ms, while the same completion carrying
// the browser's cf_clearance / __cf_bm / anthropic-device-id cookies succeeds
// (measured 2026-10-19: 30ms 429 vs 1.25s 200). Keep the operator's header
// intact instead of narrowing it to sessionKey.
func NormalizeSessionCookie(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", errors.New("claudeweb: empty session cookie")
	}
	if strings.ContainsAny(v, "\r\n") {
		return "", errors.New("claudeweb: session cookie contains CR/LF")
	}
	// Bare value, or a single name=value pair.
	if !strings.Contains(v, ";") {
		if i := strings.IndexByte(v, '='); i >= 0 {
			name := strings.TrimSpace(v[:i])
			if name != sessionCookieName {
				return "", fmt.Errorf("claudeweb: expected %s cookie, got %q", sessionCookieName, name)
			}
			v = strings.TrimSpace(v[i+1:])
			if v == "" {
				return "", errors.New("claudeweb: empty session cookie")
			}
		}
		return sessionCookieName + "=" + v, nil
	}
	// Full cookie header: keep every cookie, but require the session one.
	if CookieValue(v, sessionCookieName) == "" {
		return "", fmt.Errorf("claudeweb: cookie header has no %s", sessionCookieName)
	}
	return v, nil
}

// CookieValue returns the value of name in a Cookie header, or "".
func CookieValue(header, name string) string {
	for _, part := range strings.Split(header, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		i := strings.IndexByte(part, '=')
		if i < 0 {
			continue
		}
		if strings.TrimSpace(part[:i]) == name {
			return strings.TrimSpace(part[i+1:])
		}
	}
	return ""
}

// DeviceIDFromCookie extracts anthropic-device-id from a Cookie header. The
// browser sends it both as a cookie and as the anthropic-device-id request
// header; BrowserHeaders sets the header from this value.
func DeviceIDFromCookie(header string) string {
	return CookieValue(header, "anthropic-device-id")
}

// Organization is one entry of GET /api/organizations.
type Organization struct {
	UUID string `json:"uuid"`
	ID   any    `json:"id"` // numeric on the live API, string on some responses
	Name string `json:"name"`
}

// scalarString renders a json scalar (string or number) as a string.
func scalarString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int64:
		return strconv.FormatInt(t, 10)
	case nil:
		return ""
	default:
		return fmt.Sprint(t)
	}
}

// Client drives one claude.ai session. Conversation state: the conversation
// UUID is CLIENT-generated (reference: the id for a new conversation is a
// fresh uuid in the URL path), follow-ups chain conversation_id +
// parent_message_uuid in the payload, and state commits only after
// message_stop. One client per account+session (documented scope).
type Client struct {
	fetcher    Fetcher
	cookie     string
	deviceID   string
	timezone   string
	locale     string
	orgID      string
	convID     string // client-generated on first turn
	humanUUID  string // sent only on the first turn
	parentUUID string // last committed assistant uuid
}

// New builds a session client from a normalized cookie header.
func New(fetcher Fetcher, cookieHeader, deviceID string) *Client {
	return &Client{
		fetcher:  fetcher,
		cookie:   cookieHeader,
		deviceID: deviceID,
		timezone: time.Now().Location().String(),
		locale:   "en-US",
	}
}

// WithTimezone overrides the IANA timezone the wire sends.
func (c *Client) WithTimezone(tz string) *Client {
	if tz != "" {
		c.timezone = tz
	}
	return c
}

// NewConversation drops the conversation state so the next Generate opens a
// fresh upstream conversation. The org id stays cached.
func (c *Client) NewConversation() {
	c.convID, c.humanUUID, c.parentUUID = "", "", ""
}

// OrganizationID resolves and caches the org (also the key-test oracle:
// GET /api/organizations is one request, zero quota).
func (c *Client) OrganizationID(ctx context.Context) (string, error) {
	if c.orgID != "" {
		return c.orgID, nil
	}
	resp, err := c.fetch(ctx, http.MethodGet, apiBase+"/organizations", nil)
	if err != nil {
		return "", err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if err := classify(resp.Status, resp.Header, body); err != nil {
		return "", err
	}
	var orgs []Organization
	if err := json.Unmarshal(body, &orgs); err != nil || len(orgs) == 0 {
		return "", ErrNoOrganization
	}
	id := firstNon(orgs[0].UUID, scalarString(orgs[0].ID))
	if id == "" {
		return "", ErrNoOrganization
	}
	c.orgID = id
	return id, nil
}

// classify maps an upstream response to the typed failures (status +
// Cloudflare challenge markers, like omniroute's isClaudeWebChallenge).
func classify(status int, header http.Header, body []byte) error {
	switch {
	case status == 401:
		return ErrSessionExpired
	case status == 403:
		if isChallenge(header, body) {
			return ErrCloudflareChallenge
		}
		return ErrForbidden
	case status == 429:
		return ErrRateLimited
	case status < 200 || status >= 300:
		// Carry a short body snippet: a bare "status 400" is undebuggable
		// against a reverse-engineered wire whose errors name the field.
		return fmt.Errorf("%w: status %d: %s", ErrUnavailable, status, snippet(body))
	}
	return nil
}

// snippet renders up to 300 bytes of an upstream body for an error message.
func snippet(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// isChallenge mirrors omniroute's isClaudeWebChallenge: cf-mitigated header
// or the challenge HTML markers.
func isChallenge(header http.Header, body []byte) bool {
	if header.Get("cf-mitigated") == "challenge" {
		return true
	}
	s := strings.ToLower(string(body))
	return strings.Contains(s, "cf-browser-verification") ||
		strings.Contains(s, "just a moment") ||
		strings.Contains(s, "challenges.cloudflare.com")
}

// fetch performs a request with the browser header set + cookie.
func (c *Client) fetch(ctx context.Context, method, url string, body []byte) (*Response, error) {
	h := BrowserHeaders(c.deviceID, c.locale, "")
	if c.convID != "" {
		h.Set("Referer", "https://claude.ai/chat/"+c.convID)
	}
	h.Set("Cookie", c.cookie)
	return c.fetcher.Fetch(ctx, method, url, body, h)
}

// GenerateOptions is one completion turn.
type GenerateOptions struct {
	Model            string
	Prompt           string
	Tools            []any // Anthropic-native {type:"function",function:{name,description,input_schema}}
	ExtendedThinking bool
	Effort           string // low|medium|high|xhigh|max (validated)
}

// effortVocab is the enforced set (reference: the same list).
var effortVocab = map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true, "max": true}

// GenerateResult is the committed turn state.
type GenerateResult struct {
	Text       string
	ConvID     string
	HumanUUID  string
	AssistUUID string
	Completed  bool
}

func (c *Client) Generate(ctx context.Context, opts GenerateOptions, onDelta func(text string)) (GenerateResult, error) {
	if strings.TrimSpace(opts.Prompt) == "" {
		return GenerateResult{}, errors.New("claudeweb: empty prompt")
	}
	org, err := c.OrganizationID(ctx)
	if err != nil {
		return GenerateResult{}, err
	}

	assistant := NewUUID()
	newConversation := c.convID == ""
	if newConversation {
		c.convID = NewUUID() // client-generated; the URL path carries it
		c.humanUUID = NewUUID()
	}
	// If the FIRST turn fails, this client must not keep the speculative
	// conversation id: every later call would be a follow-up to a
	// conversation that was never created (permanent empty-body 400 until
	// the process restarts, since the bridge caches clients per account).
	committed := false
	defer func() {
		if !committed && newConversation {
			c.convID = ""
			c.humanUUID = ""
			c.parentUUID = ""
		}
	}()
	res := GenerateResult{ConvID: c.convID, AssistUUID: assistant, HumanUUID: c.humanUUID}

	url := apiBase + "/organizations/" + org + "/chat_conversations/" + c.convID + "/completion"
	resp, err := c.fetch(ctx, http.MethodPost, url, c.buildPayload(opts, assistant, newConversation))
	if err != nil {
		return res, err
	}
	defer resp.Body.Close()

	var snippet []byte
	var serverAssist string
	_, completed, perr := consumeSSE(resp.Body, func(delta string) {
		res.Text += delta
		if onDelta != nil {
			onDelta(delta)
		}
	}, &snippet, &serverAssist)
	if perr != nil || !completed {
		if resp.Status >= 400 {
			if cerr := classify(resp.Status, resp.Header, snippet); cerr != nil {
				return res, cerr
			}
		}
		if perr != nil {
			return res, perr
		}
		return res, ErrStreamCut
	}
	// Commit only now. The next turn's parent MUST be the server-assigned
	// assistant uuid from message_start — the one we generated is only a
	// request-side placeholder and chaining it 400s on the follow-up.
	c.parentUUID = serverAssist
	if c.parentUUID == "" {
		c.parentUUID = assistant
	}
	res.AssistUUID = c.parentUUID
	res.Completed = true
	committed = true
	return res, nil
}

// buildPayload assembles the completion body exactly like the reference:
// prompt flatten is the caller's, effort map, styles block, uuid turn
// markers (human only on the first turn; conversation_id + parent on
// follow-ups), create_conversation_params on the first turn.
func (c *Client) buildPayload(opts GenerateOptions, assistantUUID string, newConversation bool) []byte {
	effort, thinking := "low", "off"
	m := strings.ToLower(opts.Model)
	switch {
	case strings.Contains(m, "opus-5"):
		effort, thinking = "high", "auto"
	case opts.ExtendedThinking:
		effort, thinking = firstNon(opts.Effort, "high"), "extended"
	case effortVocab[strings.ToLower(strings.TrimSpace(opts.Effort))]:
		effort = strings.ToLower(strings.TrimSpace(opts.Effort))
	}
	tools := make([]any, 0, len(opts.Tools))
	for _, t := range opts.Tools {
		fn, ok := t.(map[string]any)
		if !ok {
			continue
		}
		f, ok := fn["function"].(map[string]any)
		if !ok {
			continue
		}
		name, _ := f["name"].(string)
		if name == "" {
			continue
		}
		entry := map[string]any{"name": name}
		if desc, ok := f["description"].(string); ok && desc != "" {
			entry["description"] = desc
		}
		params := f["parameters"]
		if params == nil {
			params = f["input_schema"]
		}
		if params != nil {
			entry["input_schema"] = params
		}
		tools = append(tools, entry)
	}

	uuids := map[string]any{}
	if newConversation {
		uuids["human_message_uuid"] = c.humanUUID
	} else {
		// Follow-ups carry the conversation id, the server-assigned parent,
		// AND a fresh human uuid for this turn — omitting the human uuid is
		// an empty-body 400 on the live wire.
		uuids["conversation_id"] = c.convID
		uuids["human_message_uuid"] = NewUUID()
		if c.parentUUID != "" {
			uuids["parent_message_uuid"] = c.parentUUID
		}
	}
	uuids["assistant_message_uuid"] = assistantUUID

	body := map[string]any{
		"prompt":   opts.Prompt,
		"model":    opts.Model,
		"timezone": c.timezone,
		"personalized_styles": []any{map[string]any{
			"type": "default", "key": "Default", "name": "Normal",
			"nameKey": "normal_style_name", "prompt": "Normal\n",
			"summary": "Default responses from Claude", "summaryKey": "normal_style_summary",
			"isDefault": true,
		}},
		"locale":             c.locale,
		"tools":              tools,
		"turn_message_uuids": uuids,
		"attachments":        []any{},
		"effort":             effort,
		"files":              []any{},
		"sync_sources":       []any{},
		"rendering_mode":     "messages",
		"thinking_mode":      thinking,
	}
	if newConversation {
		body["create_conversation_params"] = map[string]any{
			"name": "", "model": opts.Model, "include_conversation_preferences": true,
			"paprika_mode": nil, "compass_mode": nil, "is_temporary": false,
			"enabled_imagine": true, "tool_search_mode": "auto",
		}
	}
	out, _ := json.Marshal(body)
	return out
}

// consumeSSE parses the completion stream: data: frames with
// content_block_delta/text_delta deltas; message_stop ends the turn;
// error events fail the turn. The body snippet (bounded) is captured for
// classification when the status is >= 400.
func consumeSSE(body io.Reader, onDelta func(string), snippet *[]byte, serverAssist *string) (string, bool, error) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	var text strings.Builder
	for sc.Scan() {
		line := sc.Text()
		if *snippet != nil && len(*snippet) < 4096 {
			*snippet = append(*snippet, line...)
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		switch ev["type"] {
		case "content_block_delta":
			delta, ok := ev["delta"].(map[string]any)
			if !ok {
				continue
			}
			if delta["type"] == "text_delta" {
				if t, ok := delta["text"].(string); ok && t != "" {
					text.WriteString(t)
					onDelta(t)
				}
			}
		case "message_start":
			// The server, not the client, owns the assistant message uuid.
			// Capture it for the next turn's parent_message_uuid.
			if m, ok := ev["message"].(map[string]any); ok {
				if u, ok := m["uuid"].(string); ok && u != "" && serverAssist != nil {
					*serverAssist = u
				}
			}
		case "message_stop":
			return text.String(), true, nil
		case "error":
			msg, _ := ev["message"].(string)
			if msg == "" {
				msg = "stream error"
			}
			return text.String(), false, errors.New("claudeweb: " + msg)
		}
	}
	if err := sc.Err(); err != nil {
		return text.String(), false, err
	}
	return text.String(), false, nil
}

// NewUUID is a v4 uuid string (crypto-grade randomness, stdlib only).
func NewUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("claudeweb: crypto/rand unavailable: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	s := hex.EncodeToString(b[:])
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}
