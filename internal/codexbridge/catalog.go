package codexbridge

import (
	"context"
	"encoding/json"
	"math"
	"time"
)

// Model is one catalog row from model/list.
type Model struct {
	ID            string
	DisplayName   string
	Hidden        bool
	IsDefault     bool
	DefaultEffort string
	Efforts       []string
}

// Window is one plan window from account/rateLimits/read.
type Window struct {
	UsedPercent        float64 `json:"usedPercent"`
	WindowDurationMins int64   `json:"windowDurationMins"`
	ResetsAt           int64   `json:"resetsAt"`
}

// RateLimits is the plan's state: primary is the 300-minute window and
// secondary the 10,080-minute (weekly) one on Plus (measured 2026-10-23).
type RateLimits struct {
	PlanType  string  `json:"planType"`
	Primary   *Window `json:"primary"`
	Secondary *Window `json:"secondary"`
}

// Models lists every model the login serves, hidden ones included (operator
// decision §4.4).
func (r *Runner) Models(ctx context.Context) ([]Model, error) {
	if r.Channel == ChannelExec {
		return nil, errUnavailableOnExec("the model catalog")
	}
	var out []Model
	err := r.Session(ctx, func(ctx context.Context, c *rpcConn) error {
		cursor := ""
		for page := 0; page < 20; page++ {
			params := map[string]any{"includeHidden": true}
			if cursor != "" {
				params["cursor"] = cursor
			}
			raw, err := c.call(ctx, "model/list", params)
			if err != nil {
				return err
			}
			var resp struct {
				Data []struct {
					ID                        string `json:"id"`
					DisplayName               string `json:"displayName"`
					Hidden                    bool   `json:"hidden"`
					IsDefault                 bool   `json:"isDefault"`
					DefaultReasoningEffort    string `json:"defaultReasoningEffort"`
					SupportedReasoningEfforts []struct {
						ReasoningEffort string `json:"reasoningEffort"`
					} `json:"supportedReasoningEfforts"`
				} `json:"data"`
				NextCursor *string `json:"nextCursor"`
			}
			if err := json.Unmarshal(raw, &resp); err != nil {
				return err
			}
			for _, m := range resp.Data {
				row := Model{ID: m.ID, DisplayName: m.DisplayName, Hidden: m.Hidden,
					IsDefault: m.IsDefault, DefaultEffort: m.DefaultReasoningEffort}
				for _, e := range m.SupportedReasoningEfforts {
					row.Efforts = append(row.Efforts, e.ReasoningEffort)
				}
				out = append(out, row)
			}
			if resp.NextCursor == nil || *resp.NextCursor == "" {
				return nil
			}
			cursor = *resp.NextCursor
		}
		return nil
	})
	return out, err
}

// RateLimits reads the plan windows. It costs a child start (~0.2s) and no
// plan quota: the read is answered from the account, not by a model.
func (r *Runner) RateLimits(ctx context.Context) (RateLimits, error) {
	if r.Channel == ChannelExec {
		return RateLimits{}, errUnavailableOnExec("the plan-window read")
	}
	var out RateLimits
	err := r.Session(ctx, func(ctx context.Context, c *rpcConn) error {
		raw, err := c.call(ctx, "account/rateLimits/read", nil)
		if err != nil {
			return err
		}
		var resp struct {
			RateLimits RateLimits `json:"rateLimits"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			return err
		}
		out = resp.RateLimits
		return nil
	})
	return out, err
}

// UsageBody renders the plan windows in the percent quota shape the provider
// cards read ({"usage":{"rolling":{percent,resetsAt},"weekly":{…}}},
// provider.ClassifyQuota). A window of a day or less is "rolling"; longer is
// "weekly". ok is false when the login reported no window.
func UsageBody(rl RateLimits, now time.Time) ([]byte, bool) {
	usage := map[string]any{}
	for _, w := range []*Window{rl.Primary, rl.Secondary} {
		if w == nil {
			continue
		}
		shape := "weekly"
		if w.WindowDurationMins <= 24*60 {
			shape = "rolling"
		}
		entry := map[string]any{"percent": math.Round(w.UsedPercent*100) / 100,
			"windowMinutes": w.WindowDurationMins}
		if w.ResetsAt > 0 {
			entry["resetsAt"] = time.Unix(w.ResetsAt, 0).UTC().Format(time.RFC3339)
		}
		usage[shape] = entry
	}
	if len(usage) == 0 {
		return nil, false
	}
	b, err := json.Marshal(map[string]any{
		"usage":       usage,
		"plan":        rl.PlanType,
		"source":      "codex app-server account/rateLimits/read",
		"observed_at": now.UTC().Format(time.RFC3339),
	})
	return b, err == nil
}
