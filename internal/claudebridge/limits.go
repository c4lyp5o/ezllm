package claudebridge

import (
	"encoding/json"
	"math"
	"sync"
	"time"
)

// Limits keeps the plan's window state as the CLI last reported it. Every run
// carries a rate_limit_event whose unifiedWindows hold five_hour and seven_day
// utilization as a FRACTION (measured 2026-10-23: 0.01 and 0.1 on a plan with a
// week of agent traffic) plus unix-second reset times. There is no headless
// read without a call, so the reading is only as fresh as the last request;
// observed_at says how fresh.
type Limits struct {
	mu   sync.Mutex
	info map[string]any
	at   time.Time
}

// Observe records one run's rate_limit_info. A run without one (an early
// failure) leaves the previous reading in place.
func (l *Limits) Observe(info map[string]any, now time.Time) {
	if l == nil || len(info) == 0 {
		return
	}
	if _, ok := info["unifiedWindows"].(map[string]any); !ok {
		return
	}
	l.mu.Lock()
	l.info, l.at = info, now.UTC()
	l.mu.Unlock()
}

// UsageBody renders the last reading in the percent quota shape ezllm's
// provider cards already read ({"usage":{"rolling":{percent,resetsAt},
// "weekly":{…}}}, provider.ClassifyQuota). ok is false until a run reported one.
func (l *Limits) UsageBody() (body []byte, ok bool) {
	if l == nil {
		return nil, false
	}
	l.mu.Lock()
	info, at := l.info, l.at
	l.mu.Unlock()
	windows, _ := info["unifiedWindows"].(map[string]any)
	usage := map[string]any{}
	for native, shape := range map[string]string{"five_hour": "rolling", "seven_day": "weekly"} {
		w, _ := windows[native].(map[string]any)
		frac, isNum := w["utilization"].(float64)
		if !isNum {
			continue
		}
		entry := map[string]any{"percent": math.Round(frac*10000) / 100}
		if reset, isNum := w["resetsAt"].(float64); isNum && reset > 0 {
			entry["resetsAt"] = time.Unix(int64(reset), 0).UTC().Format(time.RFC3339)
		}
		usage[shape] = entry
	}
	if len(usage) == 0 {
		return nil, false
	}
	b, err := json.Marshal(map[string]any{
		"usage":       usage,
		"source":      "claude-cli rate_limit_event",
		"observed_at": at.Format(time.RFC3339),
	})
	return b, err == nil
}
