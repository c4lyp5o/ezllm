package geminiweb

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"unicode/utf8"
)

// Slot indices into the 81-element inner_req_list (Gemini-API client.py,
// _generate). Kept as named constants — the wire layout is undocumented and
// version-fragile; every slot here is copied from the reference client.
const (
	slotMessageContent = 0
	slotLanguage       = 1
	slotMetadata       = 2
	slotSessionInit    = 6
	slotStreaming      = 7
	slotTen            = 10
	slotEleven         = 11
	slotSeventeen      = 17
	slotEighteen       = 18
	slotGem            = 19
	slotTwentySeven    = 27
	slotThirty         = 30
	slotFortyOne       = 41
	slotTemporary      = 45
	slotFiftyThree     = 53
	slotFiftyNine      = 59 // per-request uuid (upper-case)
	slotSixtyOne       = 61
	slotSixtyEight     = 68
	slotSeventyNine    = 79 // model number (mirrors the model header)
	slotEighty         = 80 // 2 = extended thinking, 1 = normal
)

const generateRPCHeader = "x-goog-ext-525005358-jspb"

// Text cleanup (constants.py): artifact placeholder links Gemini embeds
// where a rendered attachment belongs.
var (
	reCardContent = regexp.MustCompile(`^https?://googleusercontent\.com/card_content/\d+`)
	reArtifacts   = regexp.MustCompile(`https?://googleusercontent\.com/(?:\w+/)+\d+\n*`)
)

// Server error codes (constants.py ErrorCode) surfaced verbatim; unknown
// codes carry their numeric value rather than a guessed label.
type ErrUpstream struct {
	Code int
	What string
}

func (e ErrUpstream) Error() string {
	if e.What != "" {
		return fmt.Sprintf("geminiweb: upstream error %d (%s)", e.Code, e.What)
	}
	return fmt.Sprintf("geminiweb: upstream error %d", e.Code)
}

func upstreamErrorName(code int) string {
	switch code {
	case 1013:
		return "temporary error (disappears shortly)"
	case 1037:
		return "usage limit exceeded"
	case 1050:
		return "model inconsistent"
	case 1052:
		return "model header invalid"
	case 1060:
		return "IP temporarily blocked"
	}
	return ""
}

// GenerateOptions is one generation turn.
type GenerateOptions struct {
	Model            *Model // required (header + model number)
	Prompt           string
	Temporary        bool
	ExtendedThinking bool
	Language         string   // default "en"
	SessionID        string   // folded into the model header
	ChatMetadata     []string // optional [cid, rid, rcid]; nil = DEFAULT_METADATA
	GemID            string   // optional gem id
}

// Delta is one streamed event (text or thoughts, whichever changed).
type Delta struct {
	Text     string
	Thoughts string
}

// GenerateResult is the committed turn state after the stream ends.
type GenerateResult struct {
	Text      string
	Thoughts  string
	CID       string
	RID       string
	RCID      string
	Completed bool
}

// Generate runs one streaming turn. onDelta fires per candidate change
// (text and/or thoughts); the final result carries the converged state.
// Conversation continuity is the CALLER's job: pass ChatMetadata
// [cid, rid, rcid] from the previous turn's result for a follow-up.
func (c *Client) Generate(ctx context.Context, opts GenerateOptions, onDelta func(Delta)) (GenerateResult, error) {
	if opts.Model == nil {
		return GenerateResult{}, errors.New("geminiweb: Generate requires a model (use GetUserStatus to discover one)")
	}
	inner, err := c.buildInnerReqList(opts)
	if err != nil {
		return GenerateResult{}, err
	}
	form := url.Values{}
	form.Set("at", c.tokens.accessToken)
	form.Set("f.req", doubleEncode(inner))

	q := c.queryParams("c")
	q.Set("_reqid", fmt.Sprint(atomic.AddInt32(&c.reqID, 100000)))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+endpointGenerate+"?"+q.Encode(), strings.NewReader(form.Encode()))
	if err != nil {
		return GenerateResult{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=utf-8")
	req.Header.Set("Origin", "https://gemini.google.com")
	req.Header.Set("Referer", "https://gemini.google.com/")
	req.Header.Set("X-Same-Domain", "1")
	for k, v := range c.modelHeadersWithTurn(*opts.Model, opts.SessionID, opts.ExtendedThinking) {
		req.Header.Set(k, v)
	}
	req.Header.Set(generateRPCHeader, fmt.Sprintf(`["%s",1]`, strings.ToUpper(newUUID())))
	c.applyCookies(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return GenerateResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return GenerateResult{}, fmt.Errorf("geminiweb: generate: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return c.consumeStream(resp.Body, onDelta)
}

// buildInnerReqList assembles the 81-slot payload exactly as the Python
// client does — sparse slots matter; nil vs missing is not equivalent on
// the wire.
func (c *Client) buildInnerReqList(opts GenerateOptions) ([]any, error) {
	lang := firstNonEmpty(opts.Language, "en")
	inner := make([]any, 81)
	inner[slotMessageContent] = []any{opts.Prompt, 0, nil, []any{}, nil, nil, 0}
	inner[slotLanguage] = []any{lang}
	if len(opts.ChatMetadata) == 3 {
		meta := make([]any, 3)
		for i, s := range opts.ChatMetadata {
			meta[i] = s
		}
		inner[slotMetadata] = meta
	} else {
		inner[slotMetadata] = defaultMetadata()
	}
	inner[slotSessionInit] = []any{1}
	inner[slotStreaming] = 1
	inner[slotTen] = 1
	inner[slotEleven] = 0
	inner[slotSeventeen] = []any{[]any{0}}
	inner[slotEighteen] = 0
	if opts.GemID != "" {
		inner[slotGem] = opts.GemID
	}
	inner[slotTwentySeven] = 1
	inner[slotThirty] = []any{4}
	inner[slotFortyOne] = []any{1}
	if opts.Temporary {
		inner[slotTemporary] = 1
	}
	inner[slotFiftyThree] = 0
	inner[slotFiftyNine] = strings.ToUpper(newUUID())
	inner[slotSixtyOne] = []any{}
	inner[slotSixtyEight] = 1
	inner[slotSeventyNine] = opts.Model.ModelNumber
	if opts.ExtendedThinking {
		inner[slotEighty] = 2
	} else {
		inner[slotEighty] = 1
	}
	return inner, nil
}

// modelHeadersWithTurn ports the per-turn header mutation: the catalog
// header, then append (2|1) thinking flag and the session id.
func (c *Client) modelHeadersWithTurn(m Model, sessionID string, extendedThinking bool) map[string]string {
	base := m.modelHeader()
	raw, ok := base[modelHeaderKey]
	if !ok {
		return base
	}
	var arr []any
	if err := json.Unmarshal([]byte(raw), &arr); err != nil {
		return base // malformed header from the catalog: send as-is
	}
	if extendedThinking {
		arr = append(arr, 2)
	} else {
		arr = append(arr, 1)
	}
	arr = append(arr, sessionID)
	out, err := json.Marshal(arr)
	if err == nil {
		base[modelHeaderKey] = string(out)
	}
	return base
}

// doubleEncode is the outer f.req envelope: [null, "<inner json>"] — the
// inner payload is a JSON *string*, not an inlined array (the endpoint
// parses the second element with json.loads).
func doubleEncode(inner []any) string {
	innerJSON, err := json.Marshal(inner)
	if err != nil {
		return "[null,\"[]\"]"
	}
	outer, err := json.Marshal([]any{nil, string(innerJSON)})
	if err != nil {
		return "[null,\"[]\"]"
	}
	return string(outer)
}

// consumeStream incrementally decodes the response body and turns wrb.fr
// frames into deltas. The UTF-8 decoder is incremental because the frame
// boundary can split a multi-byte rune across HTTP chunks.
func (c *Client) consumeStream(body io.Reader, onDelta func(Delta)) (GenerateResult, error) {
	var fp FrameParser
	dec := &utf8Incremental{}
	lastTexts := map[string]string{} // per-rcid cumulative (rcid re-sends are real)
	lastThoughts := map[string]string{}
	res := GenerateResult{}

	buf := make([]byte, 32<<10)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			text := dec.decode(buf[:n])
			if text == "" {
				if err != nil {
					break
				}
				continue
			}
			frames, ferr := fp.Feed([]byte(text))
			for _, payload := range frames {
				stop, perr := c.consumePart(payload, lastTexts, lastThoughts, onDelta, &res)
				if perr != nil {
					return res, perr
				}
				if stop {
					return res, nil // final chunk: the turn is closed
				}
			}
			if ferr != nil {
				return res, ferr
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return res, err
		}
	}
	return res, nil
}

// consumePart handles one frame payload (the JSPB part array). stop=true
// means the final-chunk marker was seen and the turn is closed.
func (c *Client) consumePart(payload string, lastTexts, lastThoughts map[string]string, onDelta func(Delta), res *GenerateResult) (bool, error) {
	var part []any
	if err := json.Unmarshal([]byte(payload), &part); err != nil {
		return false, nil // not JSON: skip (heartbeat/keep-alive frames happen)
	}
	// Error code lives at part[5][2][0][1][0]; an absent slot is normal.
	if code, ok := nestedInt(part, 5, 2, 0, 1, 0); ok && code != 0 {
		return false, ErrUpstream{Code: code, What: upstreamErrorName(code)}
	}
	bodyStr, _ := indexValue(part, 2).(string)
	if bodyStr == "" {
		return false, nil
	}
	var partJSON []any
	if err := json.Unmarshal([]byte(bodyStr), &partJSON); err != nil {
		return false, nil
	}
	// Metadata: [cid, rid].
	if meta, ok := indexValue(partJSON, 1).([]any); ok && len(meta) >= 2 {
		if cid, ok := meta[0].(string); ok && cid != "" {
			res.CID = cid
		}
		if rid, ok := meta[1].(string); ok && rid != "" {
			res.RID = rid
		}
	}
	// part_json[25] (string) = final chunk marker: the turn is saved and
	// closed. NOTE: the final-chunk part still carries candidate data on the
	// real wire — process the candidates first, then stop.
	finalChunk, _ := indexValue(partJSON, 25).(string)
	candidates, _ := indexValue(partJSON, 4).([]any)
	for _, cAny := range candidates {
		cand, ok := cAny.([]any)
		if !ok {
			continue
		}
		rcid := str(cand, 0)
		text := cleanText(str(cand, 1, 0), cand)
		thoughts := str(cand, 37, 0, 0)
		completed := false
		if code, ok := nestedInt(cand, 8, 0); ok && code == 2 {
			completed = true
		}

		var d Delta
		if text != lastTexts[rcid] {
			d.Text = suffixDelta(lastTexts[rcid], text)
			res.Text = text
			lastTexts[rcid] = text
		}
		if thoughts != lastThoughts[rcid] {
			d.Thoughts = suffixDelta(lastThoughts[rcid], thoughts)
			res.Thoughts = thoughts
			lastThoughts[rcid] = thoughts
		}
		if d.Text != "" || d.Thoughts != "" {
			onDelta(d)
		}
		if rcid != "" {
			res.RCID = rcid
		}
		if completed {
			res.Completed = true
		}
	}
	return finalChunk != "", nil
}

// cleanText ports _parse_candidate: card-content fallback to slot 22, then
// artifact-link stripping.
func cleanText(text string, cand []any) string {
	if reCardContent.MatchString(text) {
		if alt := str(cand, 22, 0); alt != "" {
			text = alt
		}
	}
	return reArtifacts.ReplaceAllString(text, "")
}

// suffixDelta emits only the appended tail — Gemini re-sends cumulative text
// per candidate, and markdown cleanup can rewrite earlier bytes.
func suffixDelta(prev, cur string) string {
	if cur == prev {
		return ""
	}
	if len(cur) > len(prev) && cur[:len(prev)] == prev {
		return cur[len(prev):]
	}
	return cur // non-prefix rewrite (cleanup): emit the whole value
}

// defaultMetadata is the zero-turn conversation marker.
func defaultMetadata() []any {
	return []any{"", "", "", nil, nil, nil, nil, nil, nil, ""}
}

// ── tiny navigation helpers over decoded JSON arrays ────────────────────────

// str reads a nested string path (arr[i0][i1]…) returning "" on any miss.
func str(arr []any, path ...int) string {
	var cur any = arr
	for _, i := range path {
		next := indexValue(cur, i)
		if next == nil {
			return ""
		}
		cur = next
	}
	s, _ := cur.(string)
	return s
}

func nestedInt(arr []any, path ...int) (int, bool) {
	var cur any = arr
	for _, i := range path {
		next := indexValue(cur, i)
		if next == nil {
			return 0, false
		}
		cur = next
	}
	return toInt(cur)
}

// utf8Incremental turns byte chunks into strings without splitting runes:
// an incomplete multi-byte sequence at a chunk tail is held back until the
// next read completes it.
type utf8Incremental struct {
	pending []byte
}

func (u *utf8Incremental) decode(chunk []byte) string {
	buf := make([]byte, 0, len(u.pending)+len(chunk))
	buf = append(buf, u.pending...)
	buf = append(buf, chunk...)
	consumed := 0
	for consumed < len(buf) {
		// FullRune is the correct tail test: an incomplete sequence is not
		// yet a rune (hold it), but an invalid byte IS (width-1 error rune,
		// pass through). DecodeRune alone cannot tell them apart when the
		// partial rune is the whole remaining buffer.
		if !utf8.FullRune(buf[consumed:]) {
			break
		}
		_, size := utf8.DecodeRune(buf[consumed:])
		consumed += size
	}
	u.pending = append([]byte(nil), buf[consumed:]...)
	return string(buf[:consumed])
}

// newUUID is a v4 uuid string (crypto-grade randomness, stdlib only).
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Deterministic fallback should never happen; surface rather than
		// silently sending a colliding request uuid.
		panic("geminiweb: crypto/rand unavailable: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
