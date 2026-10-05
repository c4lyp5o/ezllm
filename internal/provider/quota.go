package provider

import (
	"crypto/rand"
	"encoding/json"
	"io"
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
