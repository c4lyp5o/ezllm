package geminiweb

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// RPC ids (constants.py:GRPC).
const rpcGetUserStatus = "otAQ7b" // status + model catalog, zero quota

// Header keys for model selection (constants.py).
const (
	modelHeaderKey = "x-goog-ext-525001261-jspb"
	headerAux1     = "x-goog-ext-73010989-jspb"
	headerAux2     = "x-goog-ext-73010990-jspb"
)

// AccountStatus codes (constants.py:AccountStatus). Only the ones that
// change operator behavior are named; unknown codes map to ACCOUNT_REJECTED
// exactly like the Python client.
type AccountStatus struct {
	Code        int
	Name        string
	Description string
}

var (
	statusAvailable          = AccountStatus{1000, "AVAILABLE", "Account is authorized and has normal access."}
	statusUnauthenticated    = AccountStatus{1016, "UNAUTHENTICATED", "Session is not authenticated or cookies have expired."}
	statusTOSPending         = AccountStatus{1040, "TOS_PENDING", "You need to accept the latest Terms of Service to continue."}
	statusTOSOutOfDate       = AccountStatus{1042, "TOS_OUT_OF_DATE", "Terms of Service are out of date; please accept the new ones."}
	statusRejectedByGuardian = AccountStatus{1054, "ACCOUNT_REJECTED_BY_GUARDIAN", "Access is blocked by a parent or guardian."}
	statusGuardianApproval   = AccountStatus{1057, "GUARDIAN_APPROVAL_REQUIRED", "Access requires parent or guardian approval."}
	statusLocationRejected   = AccountStatus{1060, "LOCATION_REJECTED", "Gemini is not currently supported in your country/region."}
	statusRejected           = AccountStatus{1021, "ACCOUNT_REJECTED", "Account access is rejected. Please check your Google Account settings."}
	statusUntrusted          = AccountStatus{1033, "ACCOUNT_UNTRUSTED", "Account did not pass safety or trust checks for some features."}
)

func accountStatusFromCode(code int) AccountStatus {
	switch code {
	case 1000:
		return statusAvailable
	case 1014:
		return AccountStatus{1014, "ACCESS_TEMPORARILY_UNAVAILABLE", "Access is restricted, possibly due to regional or temporary session issues."}
	case 1016:
		return statusUnauthenticated
	case 1021:
		return statusRejected
	case 1033:
		return statusUntrusted
	case 1040:
		return statusTOSPending
	case 1042:
		return statusTOSOutOfDate
	case 1054:
		return statusRejectedByGuardian
	case 1057:
		return statusGuardianApproval
	case 1060:
		return statusLocationRejected
	default:
		return statusRejected
	}
}

// Model is one entry of the account's model catalog
// (AvailableModel in the Python client). The Header fields are what dial a
// generation for this model — built per request, never hardcoded.
type Model struct {
	ID            string
	Name          string
	DisplayName   string
	Description   string
	Capacity      int
	CapacityField int
	ModelNumber   int
	Available     bool
	Aliases       []string
}

// modelHeader replicates build_model_header(): the model JSPB header plus
// its two aux headers. capacityTail is the raw capacity or the 13-field
// variant.
func (m Model) modelHeader() map[string]string {
	tail := strconv.Itoa(m.Capacity)
	if m.CapacityField == 13 {
		tail = "null," + strconv.Itoa(m.Capacity)
	}
	id, _ := json.Marshal(m.ID) // a JSON array: %q is not JSON for control bytes / invalid UTF-8
	return map[string]string{
		modelHeaderKey: fmt.Sprintf(`[1,null,null,null,%s,null,null,0,[4,5,6,8],null,null,%s,null,null,%d]`, id, tail, m.ModelNumber),
		headerAux1:     "[0]",
		headerAux2:     "[0,0,0]",
	}
}

// UserStatus is the GET_USER_STATUS result: the account verdict plus the
// discovered catalog.
type UserStatus struct {
	Status AccountStatus
	Models []Model
}

// batchExecute POSTs RPCs to batchexecute and returns the raw response text
// (which carries Google's ")]}'" XSSI prefix). Parsing happens per RPC.
func (c *Client) batchExecute(ctx context.Context, rpcs []rpcCall) (string, error) {
	// Outer envelope per the live browser capture (gemini.google.com HAR):
	//   f.req = [[[rpcid, payload, null, "generic"]]]
	//   at   = <SNlM0e access token>
	// payload for GET_USER_STATUS is "[1]" (not "[]").
	inner := make([]any, len(rpcs))
	for i, r := range rpcs {
		inner[i] = []any{r.id, r.payload, nil, "generic"}
	}
	encoded, err := json.Marshal([]any{inner})
	if err != nil {
		return "", err
	}
	form := url.Values{}
	form.Set("at", c.tokens.accessToken)
	form.Set("f.req", string(encoded))

	q := c.queryParams("")
	q.Set("rpcids", rpcs[0].id)
	q.Set("source-path", "/app")
	endpoint := c.base + endpointBatchExec + "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=utf-8")
	req.Header.Set("X-Same-Domain", "1")
	req.Header.Set("Origin", c.base)
	req.Header.Set("Referer", c.base+"/")
	c.applyCookies(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("geminiweb: batchexecute: status %d", resp.StatusCode)
	}
	return string(body), nil
}

type rpcCall struct {
	id      string
	payload string
}

// GetUserStatus is the zero-quota oracle: auth verdict + model catalog.
func (c *Client) GetUserStatus(ctx context.Context) (UserStatus, error) {
	raw, err := c.batchExecute(ctx, []rpcCall{{id: rpcGetUserStatus, payload: "[1]"}})
	if err != nil {
		return UserStatus{}, err
	}
	parts, err := extractFrames(raw)
	if err != nil {
		return UserStatus{}, err
	}
	var out UserStatus
	found := false
	// Live responses wrap the frames in an outer array: [[frame1, frame2, ...]].
	// Mocks return a bare frame. Normalize both shapes.
	var frames []any
	for _, part := range parts {
		if arr, ok := part.([]any); ok && len(arr) > 0 {
			if inner, ok := arr[0].([]any); ok && len(inner) > 0 {
				if s, _ := inner[0].(string); s == "wrb.fr" || s == "er" {
					frames = append(frames, arr...) // unwrap one level
					continue
				}
			}
		}
		frames = append(frames, part)
	}
	for _, part := range frames {
		// Frame parts are [kind, rpcid, body, ...]; keep ours.
		kind, _ := indexValue(part, 1).(string)
		if kind != rpcGetUserStatus {
			continue
		}
		bodyStr, _ := indexValue(part, 2).(string)
		var body any
		if err := json.Unmarshal([]byte(bodyStr), &body); err != nil {
			continue
		}
		// from_status_code(None) -> AVAILABLE: a missing status slot on an
		// otherwise well-formed payload reads as OK, exactly like the Python
		// client. A present non-1000 code is the account verdict.
		code := 1000
		if n, ok := toInt(indexValue(body, 14)); ok {
			code = n
		}
		out.Status = accountStatusFromCode(code)
		if out.Status == statusAvailable {
			out.Models = parseModels(body)
		}
		found = true
		break
	}
	if !found {
		return UserStatus{}, fmt.Errorf("geminiweb: GetUserStatus: no %s part in response", rpcGetUserStatus)
	}
	return out, nil
}

// extractFrames parses the batchexecute response: strip Google's ")]}'"
// prefix, then run the wrb.fr parser (parsing.py:extract_json_from_response),
// returning each frame as parsed JSON.
func extractFrames(raw string) ([]any, error) {
	content := strings.TrimPrefix(raw, ")]}'")
	content = strings.TrimLeft(content, "\r\n\t ")
	var p FrameParser
	strs, err := p.Feed([]byte(content))
	if err != nil {
		// The Python client falls back to whole-content JSON when framing
		// fails; do the same before giving up.
		var whole any
		if jerr := json.Unmarshal([]byte(strings.TrimSpace(content)), &whole); jerr == nil {
			return []any{whole}, nil
		}
		return nil, err
	}
	out := make([]any, 0, len(strs))
	for _, s := range strs {
		var v any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			continue
		}
		out = append(out, v)
	}
	return out, nil
}

// parseModels reads part_body[15] descriptors (AvailableModel.from_rpc) and
// derives capacity from part_body[16]/[17] (compute_capacity).
func parseModels(body any) []Model {
	raw, ok := indexValue(body, 15).([]any)
	if !ok {
		return nil
	}
	capacity, capacityField := computeCapacity(indexIntSlice(body, 16), indexIntSlice(body, 17))
	models := make([]Model, 0, len(raw))
	for _, item := range raw {
		m, ok := parseModel(item, capacity, capacityField)
		if !ok {
			continue
		}
		models = append(models, m)
	}
	return models
}

// parseModel ports AvailableModel.from_rpc exactly: model_id [0], category
// [1] or [10], display [11] or [19] or [1], description [12] or [2],
// model_number [17] or [9] else 1.
func parseModel(item any, capacity, capacityField int) (Model, bool) {
	id, _ := indexValue(item, 0).(string)
	if id == "" {
		return Model{}, false
	}
	category := firstNonEmpty(strAt(item, 1), strAt(item, 10))
	display := firstNonEmpty(strAt(item, 11), strAt(item, 19), strAt(item, 1))
	description := firstNonEmpty(strAt(item, 12), strAt(item, 2))
	number := 1
	if n, ok := toInt(indexValue(item, 17)); ok {
		number = n
	} else if n, ok := toInt(indexValue(item, 9)); ok {
		number = n
	}
	name, aliases := deriveNameAndAliases(id, category, display)
	return Model{
		ID:            id,
		Name:          name,
		DisplayName:   firstNonEmpty(display, category, id),
		Description:   description,
		Capacity:      capacity,
		CapacityField: capacityField,
		ModelNumber:   number,
		Available:     true,
		Aliases:       aliases,
	}, true
}

// deriveNameAndAliases ports AvailableModel._derive_name_and_aliases
// (version-agnostic, no hardcoded model names).
func deriveNameAndAliases(modelID, categoryName, displayName string) (string, []string) {
	set := map[string]struct{}{}
	add := func(s string) {
		if s != "" {
			set[s] = struct{}{}
		}
	}
	if modelID != "" {
		add(strings.ToLower(modelID))
	}
	cat := strings.TrimSpace(categoryName)
	disp := strings.TrimSpace(displayName)
	slug := func(s string) string { return strings.ToLower(strings.ReplaceAll(s, " ", "-")) }
	var major string
	if m := reVersion.FindStringSubmatch(disp); m != nil {
		major = m[1]
	}
	if cat != "" {
		catSlug := slug(cat)
		add(strings.ToLower(cat))
		add(catSlug)
		add("gemini-" + catSlug)
		if major != "" {
			add(fmt.Sprintf("gemini-%s-%s", major, catSlug))
		}
	}
	if disp != "" {
		dispSlug := slug(disp)
		add(strings.ToLower(disp))
		add(dispSlug)
		add("gemini-" + dispSlug)
	}
	primary := ""
	switch {
	case cat != "":
		primary = "gemini-" + slug(cat)
	case disp != "":
		primary = "gemini-" + slug(disp)
	default:
		primary = "gemini-" + modelID
	}
	add(primary)
	aliases := make([]string, 0, len(set))
	for a := range set {
		aliases = append(aliases, a)
	}
	sort.Strings(aliases)
	return primary, aliases
}

var reVersion = regexp.MustCompile(`(\d+)(?:\.\d+)?`)

// computeCapacity ports compute_capacity: tier flag 21/22 -> field 13,
// capability 115 -> Plus(4), tier 16 / cap 106 -> Pro(3), tier 8 / cap 19
// -> Pro(2), else Free(1).
func computeCapacity(tierFlags, capabilityFlags []int) (int, int) {
	has := func(s []int, v int) bool {
		for _, x := range s {
			if x == v {
				return true
			}
		}
		return false
	}
	if has(tierFlags, 21) {
		return 1, 13
	}
	if has(tierFlags, 22) {
		return 2, 13
	}
	if has(capabilityFlags, 115) {
		return 4, 12
	}
	if has(tierFlags, 16) || has(capabilityFlags, 106) {
		return 3, 12
	}
	if has(tierFlags, 8) || has(capabilityFlags, 19) {
		return 2, 12
	}
	return 1, 12
}

// ── JSON navigation helpers (get_nested_value, int-safe) ─────────────────────

func indexValue(data any, idx int) any {
	arr, ok := data.([]any)
	if !ok || idx < 0 || idx >= len(arr) {
		return nil
	}
	return arr[idx]
}

func indexIntSlice(data any, idx int) []int {
	arr, ok := indexValue(data, idx).([]any)
	if !ok {
		return nil
	}
	out := make([]int, 0, len(arr))
	for _, v := range arr {
		if n, ok := toInt(v); ok {
			out = append(out, n)
		}
	}
	return out
}

func strAt(data any, idx int) string {
	s, _ := indexValue(data, idx).(string)
	return s
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}
