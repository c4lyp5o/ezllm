package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/c4lyp5o/ezllm/internal/claudeweb"
)

// claudeSubCreds extracts what the claude.ai usage read needs from a pasted
// Cookie header: the whole header (Cloudflare answers a sessionKey-only
// request with a challenge; the browser's cf_clearance/__cf_bm cookies get
// through, see claudeweb.NormalizeSessionCookie) and the org id from
// lastActiveOrg. The org id goes into a URL path, so it must be a bare UUID.
func claudeSubCreds(pasted string) (cookie, org string, ok bool) {
	cookie, err := claudeweb.NormalizeSessionCookie(pasted)
	if err != nil {
		return "", "", false
	}
	org = claudeweb.CookieValue(cookie, "lastActiveOrg")
	if !claudeOrgIDRe.MatchString(org) {
		return "", "", false
	}
	return cookie, org, true
}

func (a *anthropicCompatible) claudeFetcher() claudeweb.Fetcher {
	a.fetchOnce.Do(func() {
		if a.fetcher == nil {
			a.fetcher = claudeweb.NewTLSFetcher()
		}
	})
	return a.fetcher
}

// readClaudeSubUsage reads GET claude.ai/api/organizations/{org}/usage — the
// numbers the Settings → Usage page shows, fresh without spending a request.
func (a *anthropicCompatible) readClaudeSubUsage(ctx context.Context, cookie, org string) (Quota, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	h := claudeweb.BrowserHeaders(claudeweb.DeviceIDFromCookie(cookie), "en-US", "https://claude.ai/settings/usage")
	h.Set("Accept", "*/*")
	h.Set("Cookie", cookie)
	resp, err := a.claudeFetcher().Fetch(ctx, http.MethodGet, "https://claude.ai/api/organizations/"+org+"/usage", nil, h)
	if err != nil {
		return Quota{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.Status != http.StatusOK {
		return Quota{}, fmt.Errorf("claude.ai usage: status %d", resp.Status)
	}
	if q := parseClaudeSubUsage(string(body)); q.Kind != "" {
		return tagQuotaRaw(q, "source", "claude.ai usage"), nil
	}
	return Quota{}, errors.New("claude.ai usage: unrecognised body")
}

// parseClaudeSubUsage parses the claude.ai usage body:
//
//	{"five_hour":{"utilization":1.0,"resets_at":"2026-10-26T07:30:00.18+00:00"},
//	 "seven_day":{"utilization":93.0,"resets_at":"…"},"seven_day_opus":null,…}
//
// utilization is already a percent (the CLI's rate_limit_event carries a
// fraction instead). five_hour is the rolling window, seven_day the weekly.
func parseClaudeSubUsage(body string) Quota {
	var raw map[string]any
	if err := jsonUnmarshal([]byte(body), &raw); err != nil {
		return Quota{}
	}
	q := Quota{Raw: body, ReadAt: time.Now().UTC()}
	q.PercentRolling = jsonFloat(raw, "five_hour.utilization")
	q.PercentWeekly = jsonFloat(raw, "seven_day.utilization")
	q.RollingResetsAt = claudeSubReset(raw, "five_hour.resets_at")
	q.WeeklyResetsAt = claudeSubReset(raw, "seven_day.resets_at")
	if q.PercentRolling == nil && q.PercentWeekly == nil {
		return Quota{}
	}
	q.Kind = QuotaPercent
	return q
}

// claudeSubReset normalises a reset time to UTC RFC3339, as the other percent
// oracles report; an unparseable value is passed through.
func claudeSubReset(m map[string]any, path string) string {
	s := jsonStr(m, path)
	if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(s)); err == nil {
		return t.UTC().Format(time.RFC3339)
	}
	return s
}

// tagQuotaRaw adds one string key to a quota's raw JSON so the Quota dialog
// can say where the reading came from. A raw body that is not a JSON object is
// left as it was; the error text is bounded (it never carries the cookie —
// fetch errors name the status or the transport failure only).
func tagQuotaRaw(q Quota, key, val string) Quota {
	var m map[string]any
	if err := jsonUnmarshal([]byte(q.Raw), &m); err != nil || m == nil {
		return q
	}
	if len(val) > 200 {
		val = val[:200] + "…"
	}
	m[key] = val
	if b, err := json.Marshal(m); err == nil {
		q.Raw = string(b)
	}
	return q
}
