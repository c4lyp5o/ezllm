package provider

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// randReader is the package CSPRNG, swappable in tests.
var randReader io.Reader = rand.Reader

// jsonUnmarshal is a thin indirection so tests can share the same decode path.
func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// parsePercentQuota parses the opencode-go /usage shape:
//
//	{"usage":{"rolling":{"status","percent","resetsAt"},"weekly":{…},"monthly":{…}}}
//
// Returns a zero Quota (Kind == "") when the payload does not match, which
// callers treat as ErrQuotaUnsupported rather than a hard failure.
func parsePercentQuota(body string) Quota {
	var raw map[string]any
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		return Quota{}
	}
	u, ok := raw["usage"].(map[string]any)
	if !ok {
		// some deployments omit the wrapper
		u = raw
	}
	q := Quota{Raw: body, ReadAt: time.Now().UTC()}
	q.PercentRolling = jsonFloat(u, "rolling.percent")
	q.PercentWeekly = jsonFloat(u, "weekly.percent")
	q.PercentMonthly = jsonFloat(u, "monthly.percent")
	q.RollingResetsAt = jsonStr(u, "rolling.resetsAt")
	q.WeeklyResetsAt = jsonStr(u, "weekly.resetsAt")
	q.MonthlyResetsAt = jsonStr(u, "monthly.resetsAt")
	if q.PercentRolling == nil && q.PercentWeekly == nil && q.PercentMonthly == nil {
		return Quota{}
	}
	q.Kind = QuotaPercent
	return q
}

// parseMoneyQuota parses the ssn-gpt (space.stationine.com) /usage shape:
//
//	{"balance":0.2566,"remaining":…,"unit":"USD","cost":…,"actual_cost":…,
//	 "usage":{"today":{requests,input_tokens,output_tokens,total_tokens,cost},
//	          "total":{cost,actual_cost,…}},
//	 "daily_usage":[{date,requests,input_tokens,output_tokens,cache_read_tokens,
//	                cache_write_tokens,total_tokens,cost,actual_cost}],
//	 "model_stats":[{model,requests,…,cost,actual_cost,account_cost}]}
//
// Also accepts OpenAI's classic billing shape (total_granted/total_used) and a
// percent-shaped payload, so one function covers the money-ish providers.
func parseMoneyQuota(body string) Quota {
	var raw map[string]any
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		return Quota{}
	}
	q := Quota{Raw: body, ReadAt: time.Now().UTC()}

	bal := jsonFloat(raw, "balance")
	if bal == nil {
		bal = jsonFloat(raw, "remaining")
	}
	if bal != nil {
		unit := jsonStr(raw, "unit")
		if unit == "" {
			unit = "USD"
		}
		q.Kind = QuotaMoney
		q.Balance = bal
		q.BalanceUnit = unit
		q.CostTotal = firstFloat(raw, "usage.total.cost", "usage.total.actual_cost", "cost")
		q.CostToday = firstFloat(raw, "usage.today.cost", "usage.today.actual_cost")
		if v := jsonFloat(raw, "usage.today.total_tokens"); v != nil {
			q.TokensToday = i64ptr(int64(*v))
		}
		if v := jsonFloat(raw, "usage.today.requests"); v != nil {
			q.ReqsToday = i64ptr(int64(*v))
		}
		return q
	}
	// OpenAI classic billing shape
	if g := jsonFloat(raw, "total_granted"); g != nil {
		q.Kind = QuotaMoney
		q.Balance = g
		q.BalanceUnit = "credit"
		q.CostTotal = jsonFloat(raw, "total_used")
		return q
	}
	return Quota{}
}

// firstFloat returns the first present dot-path as a float.
func firstFloat(m map[string]any, paths ...string) *float64 {
	for _, p := range paths {
		if v := jsonFloat(m, p); v != nil {
			return v
		}
	}
	return nil
}

// ClassifyQuota tries each known shape in turn and returns the first match.
// Adapters call this so a provider whose shape we haven't special-cased still
// gets recorded (with raw JSON) instead of being discarded.
func ClassifyQuota(body string) Quota {
	if q := parsePercentQuota(body); q.Kind != "" {
		return q
	}
	if q := parseMoneyQuota(body); q.Kind != "" {
		return q
	}
	return Quota{Kind: QuotaNone, Raw: body, ReadAt: time.Now().UTC()}
}

// ─────────────────────────────────────────────────────────────────────────────
// Host-scoped quota oracles — providers with no /v1/usage whose SAME inference
// key can still read a balance endpoint. Design: docs/quota-oracles-design.md.
// Every URL and parser below was live-probed 2026-10-14 with the operator's
// own keys (zero-token GETs, UA ezllm/1.0).
// ─────────────────────────────────────────────────────────────────────────────

// parseFloatString accepts a JSON number OR the quoted-number strings some
// vendors send (Vercel: {"balance":"4.3014513844"}). jsonFloat rejects
// strings, so the money parsers need this instead.
func parseFloatString(v any) *float64 {
	switch n := v.(type) {
	case float64:
		return &n
	case int:
		f := float64(n)
		return &f
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(n), 64); err == nil {
			return &f
		}
	}
	return nil
}

// quotaOracleFor resolves a host-scoped balance endpoint for an account.
// Scope = kind openai-compatible + https + EXACT hostname (the isCloudflareAI
// discipline): neither the account name/namespace nor a user-supplied path can
// trigger another provider's oracle. ok=false → the caller falls back to the
// generic {base}/usage path.
func quotaOracleFor(acct Account) (urlStr string, parse func(string) Quota, ok bool) {
	if acct.Kind != KindOpenAICompatible {
		return "", nil, false
	}
	u, err := url.Parse(strings.TrimSpace(acct.BaseURL))
	if err != nil || !strings.EqualFold(u.Scheme, "https") {
		return "", nil, false
	}
	base := strings.TrimSuffix(acct.BaseURL, "/")
	switch strings.ToLower(u.Hostname()) {
	case "api.deepseek.com":
		return base + "/user/balance", parseDeepSeekBalance, true
	case "openrouter.ai":
		return base + "/key", parseOpenRouterKey, true
	case "api.tokenrouter.com":
		// Management API, not the inference API: requires a MANAGEMENT key
		// (sk-… from the TokenRouter dashboard), not the inference key.
		return "https://api.tokenrouter.com/api/management/self/wallet", parseTokenRouterWallet, true
	case "api.commandcode.ai":
		// Cookie-gated billing read (NOT the provider API): origin-rooted,
		// authenticated with the operator's web session — see ReadQuota.
		return u.Scheme + "://" + u.Host + "/internal/billing/credits", parseCommandCodeCredits, true
	case "ai-gateway.vercel.sh":
		return base + "/credits", parseVercelCredits, true
	case "ollama.com":
		// Origin-rooted: the usage API lives at /api/usage, NOT under the
		// account base's /v1 — appending to base would 404.
		return u.Scheme + "://" + u.Host + "/api/usage?range=24h", parseOllamaUsage, true
	}
	return "", nil, false
}

// parseDeepSeekBalance parses GET /user/balance:
//
//	{"is_available":true,"balance_infos":[{"currency":"USD","total_balance":"0.09",
//	  "granted_balance":"0.00","topped_up_balance":"0.09"},{"currency":"CNY",…9.17…}]}
//
// Multi-currency and the entry ORDER is not stable across reads (observed both
// CNY-first and USD-first), so USD is preferred explicitly, else the first
// entry; the unit records WHICH currency was picked and raw keeps them all.
func parseDeepSeekBalance(body string) Quota {
	var raw struct {
		BalanceInfos []struct {
			Currency     string `json:"currency"`
			TotalBalance string `json:"total_balance"`
		} `json:"balance_infos"`
	}
	if err := jsonUnmarshal([]byte(body), &raw); err != nil || len(raw.BalanceInfos) == 0 {
		return Quota{}
	}
	pick := raw.BalanceInfos[0]
	for _, b := range raw.BalanceInfos {
		if strings.EqualFold(b.Currency, "USD") {
			pick = b
			break
		}
	}
	bal := parseFloatString(pick.TotalBalance)
	if bal == nil || pick.Currency == "" {
		return Quota{}
	}
	return Quota{
		Kind: QuotaMoney, Raw: body, ReadAt: time.Now().UTC(),
		Balance: bal, BalanceUnit: strings.ToUpper(pick.Currency),
	}
}

// parseTokenRouterWallet parses GET /api/management/self/wallet:
//
//	{"data":{"topUpBalance":3.690708,"toppedUpSpent":36.309292,
//	         "voucherEfficientAmount":0,"voucherSpent":10},
//	 "message":"","success":true}
//
// Balance = top-up balance + remaining voucher value. CostTotal = lifetime
// spend across both purses.
func parseTokenRouterWallet(body string) Quota {
	var raw struct {
		Data *struct {
			TopUpBalance           *float64 `json:"topUpBalance"`
			ToppedUpSpent          *float64 `json:"toppedUpSpent"`
			VoucherEfficientAmount *float64 `json:"voucherEfficientAmount"`
			VoucherSpent           *float64 `json:"voucherSpent"`
		} `json:"data"`
		Success bool `json:"success"`
	}
	if err := jsonUnmarshal([]byte(body), &raw); err != nil || raw.Data == nil || !raw.Success {
		return Quota{}
	}
	if raw.Data.TopUpBalance == nil && raw.Data.VoucherEfficientAmount == nil {
		return Quota{}
	}
	bal := 0.0
	if raw.Data.TopUpBalance != nil {
		bal += *raw.Data.TopUpBalance
	}
	if raw.Data.VoucherEfficientAmount != nil {
		bal += *raw.Data.VoucherEfficientAmount
	}
	cost := 0.0
	if raw.Data.ToppedUpSpent != nil {
		cost += *raw.Data.ToppedUpSpent
	}
	if raw.Data.VoucherSpent != nil {
		cost += *raw.Data.VoucherSpent
	}
	return Quota{
		Kind: QuotaMoney, Raw: body, ReadAt: time.Now().UTC(),
		Balance: &bal, BalanceUnit: "USD", CostTotal: &cost,
	}
}

// parseOpenRouterKey parses GET /api/v1/key:
//
//	{"data":{"limit":1000,"limit_remaining":999.98,"limit_reset":null,
//	  "usage":0.0167,"usage_daily":0,"usage_weekly":0.0167,…}}
//
// balance = remaining on the key's credit limit (nil for an unlimited key →
// the UI shows "–" rather than a fabricated zero), cost_total = all-time spend,
// cost_today = today's spend.
func parseOpenRouterKey(body string) Quota {
	var raw struct {
		Data *struct {
			Limit          *float64 `json:"limit"`
			LimitRemaining *float64 `json:"limit_remaining"`
			Usage          *float64 `json:"usage"`
			UsageDaily     *float64 `json:"usage_daily"`
		} `json:"data"`
	}
	if err := jsonUnmarshal([]byte(body), &raw); err != nil || raw.Data == nil {
		return Quota{}
	}
	d := raw.Data
	if d.LimitRemaining == nil && d.Usage == nil {
		return Quota{}
	}
	// limit_reset is deliberately NOT stored: it is an INTERVAL NAME
	// ("monthly"|null), not a timestamp — putting it in resets_at would show
	// the UI a non-time. It stays in raw.
	return Quota{
		Kind: QuotaMoney, Raw: body, ReadAt: time.Now().UTC(),
		Balance: d.LimitRemaining, BalanceUnit: "USD",
		CostTotal: d.Usage, CostToday: d.UsageDaily,
	}
}

// parseVercelCredits parses GET /v1/credits — both values are STRINGS:
//
//	{"balance":"4.3014513844","total_used":"0.6985486156"}
//
// A payload without a balance is not this shape (no quota to show) → zero.
func parseVercelCredits(body string) Quota {
	var raw map[string]any
	if err := jsonUnmarshal([]byte(body), &raw); err != nil {
		return Quota{}
	}
	bal := parseFloatString(raw["balance"])
	if bal == nil {
		return Quota{}
	}
	return Quota{
		Kind: QuotaMoney, Raw: body, ReadAt: time.Now().UTC(),
		Balance: bal, BalanceUnit: "USD",
		CostTotal: parseFloatString(raw["total_used"]),
	}
}

// parseOllamaUsage parses GET /api/usage?range=24h:
//
//	{"range":"24h","scope":"self","granularity":"hour","from":…,"until":…,
//	  "totals":{"request_count":11},"buckets":[…]}
//
// Ollama reports REQUEST COUNTS, no ceiling — kind=tokens with
// requests_today set, never a percent (a percentage bar against a limit the
// provider never stated would be fabricated). buckets are kept in raw.
func parseOllamaUsage(body string) Quota {
	var raw struct {
		Range  string `json:"range"`
		Totals *struct {
			RequestCount int64 `json:"request_count"`
		} `json:"totals"`
	}
	if err := jsonUnmarshal([]byte(body), &raw); err != nil || raw.Totals == nil {
		return Quota{}
	}
	q := Quota{Kind: QuotaTokens, Raw: body, ReadAt: time.Now().UTC()}
	q.ReqsToday = &raw.Totals.RequestCount
	return q
}

// commandCodeSessionCookie is the better-auth session cookie the Command Code
// web app authenticates its billing reads with.
const commandCodeSessionCookie = "__Secure-commandcode_prod_.session_token"

// commandCodeCookieHeader builds the Cookie header from whatever the operator
// pasted: the bare token value, "name=value", or a full browser cookie string
// (the session_token pair is picked out of it). "" when nothing usable.
func commandCodeCookieHeader(pasted string) string {
	pasted = strings.TrimSpace(pasted)
	if pasted == "" {
		return ""
	}
	for _, part := range strings.Split(pasted, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, commandCodeSessionCookie+"=") {
			return part
		}
	}
	if !strings.Contains(pasted, ";") && !strings.Contains(pasted, " ") {
		return commandCodeSessionCookie + "=" + pasted
	}
	return ""
}

// parseCommandCodeCredits parses GET /internal/billing/credits:
//
//	{"credits":{"monthlyCredits":45.75,"monthlyCreditsGranted":70,…},
//	 "windowLimits":{"fiveHour":{"used":0,"cap":14,"resetAt":0},
//	                 "weekly":{"used":2.08,"cap":35,"resetAt":1792986424541}},…}
//
// Percent shape: rolling = 5h window used/cap, weekly = weekly used/cap,
// monthly = share of the monthly grant already spent (monthlyCredits is the
// REMAINING balance). resetAt is epoch milliseconds, 0 when the window is idle.
func parseCommandCodeCredits(body string) Quota {
	type window struct {
		Used    *float64 `json:"used"`
		Cap     *float64 `json:"cap"`
		ResetAt float64  `json:"resetAt"`
	}
	var raw struct {
		Credits *struct {
			Monthly *float64 `json:"monthlyCredits"`
			Granted *float64 `json:"monthlyCreditsGranted"`
		} `json:"credits"`
		WindowLimits *struct {
			FiveHour *window `json:"fiveHour"`
			Weekly   *window `json:"weekly"`
		} `json:"windowLimits"`
	}
	if err := jsonUnmarshal([]byte(body), &raw); err != nil || (raw.Credits == nil && raw.WindowLimits == nil) {
		return Quota{}
	}
	pct := func(used, limit *float64) *float64 {
		if used == nil || limit == nil || *limit <= 0 {
			return nil
		}
		v := *used / *limit * 100
		return &v
	}
	resets := func(ms float64) string {
		if ms <= 0 {
			return ""
		}
		return time.UnixMilli(int64(ms)).UTC().Format(time.RFC3339)
	}
	q := Quota{Raw: body, ReadAt: time.Now().UTC()}
	if w := raw.WindowLimits; w != nil {
		if w.FiveHour != nil {
			q.PercentRolling = pct(w.FiveHour.Used, w.FiveHour.Cap)
			q.RollingResetsAt = resets(w.FiveHour.ResetAt)
		}
		if w.Weekly != nil {
			q.PercentWeekly = pct(w.Weekly.Used, w.Weekly.Cap)
			q.WeeklyResetsAt = resets(w.Weekly.ResetAt)
		}
	}
	if c := raw.Credits; c != nil && c.Monthly != nil && c.Granted != nil && *c.Granted > 0 {
		spent := *c.Granted - *c.Monthly
		q.PercentMonthly = pct(&spent, c.Granted)
	}
	if q.PercentRolling == nil && q.PercentWeekly == nil && q.PercentMonthly == nil {
		return Quota{}
	}
	q.Kind = QuotaPercent
	return q
}

// CommandCodeCookieExpiry reads the web session's expiry from a pasted full
// cookie string: the better-auth session_data cookie is base64 JSON carrying
// session.session.expiresAt. ok=false when the paste has no session_data (a
// bare token carries no expiry) or it does not parse — then no hint is shown.
// The value is the expiry AT PASTE TIME: the session slides while it is used.
func CommandCodeCookieExpiry(pasted string) (time.Time, bool) {
	const name = "__Secure-commandcode_prod_.session_data="
	for _, part := range strings.Split(pasted, ";") {
		part = strings.TrimSpace(part)
		if !strings.HasPrefix(part, name) {
			continue
		}
		v, err := url.QueryUnescape(strings.TrimPrefix(part, name))
		if err != nil {
			return time.Time{}, false
		}
		v = strings.TrimRight(v, "=")
		b, err := base64.RawStdEncoding.DecodeString(v)
		if err != nil {
			if b, err = base64.RawURLEncoding.DecodeString(v); err != nil {
				return time.Time{}, false
			}
		}
		var raw struct {
			Session struct {
				Session struct {
					ExpiresAt string `json:"expiresAt"`
				} `json:"session"`
			} `json:"session"`
		}
		if json.Unmarshal(b, &raw) != nil {
			return time.Time{}, false
		}
		t, err := time.Parse(time.RFC3339Nano, raw.Session.Session.ExpiresAt)
		return t, err == nil
	}
	return time.Time{}, false
}
