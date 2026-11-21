package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/c4lyp5o/ezllm/internal/jsonbody"
	"github.com/c4lyp5o/ezllm/internal/provider"
)

// countTokensTimeout bounds one upstream count. Claude Code asks for counts
// while it plans a request; a slow answer is worse than the local estimate.
const countTokensTimeout = 10 * time.Second

// countTokensMaxHops caps how many hops one count may try before the caller
// falls back to its estimate.
const countTokensMaxHops = 3

// CountTokens asks the candidate hops that speak the Anthropic wire natively
// for the prompt's input tokens (POST /v1/messages/count_tokens), in order,
// and returns the first answer. ok is false when none answered: translated
// hops, bridges and gateways without the endpoint all land there, and the
// caller estimates. A count is free and says nothing about a hop's health, so
// failures never cool a hop down and no ledger row is written.
func (d *Dispatcher) CountTokens(ctx context.Context, r *http.Request, routes []Route, doc *jsonbody.BodyDoc) (tokens int64, served Route, ok bool) {
	tried := 0
	now := time.Now()
	for _, rt := range routes {
		if tried == countTokensMaxHops {
			break
		}
		if rt.EffectiveWire() != provider.SurfaceAnthropic || d.parked(rt, now) {
			continue
		}
		tried++
		if n, err := d.countOnce(ctx, r, rt, doc); err == nil {
			return n, rt, true
		}
	}
	return 0, Route{}, false
}

func (d *Dispatcher) countOnce(ctx context.Context, r *http.Request, rt Route, doc *jsonbody.BodyDoc) (int64, error) {
	ad, err := d.registry.Get(rt.Account.Kind)
	if err != nil {
		return 0, err
	}
	body, err := doc.WithModel(rt.Model)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, countTokensTimeout)
	defer cancel()
	url := provider.UpstreamURL(rt.Account.BaseURL, rt.Account.Kind, provider.SurfacePath(provider.SurfaceAnthropic)+"/count_tokens")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	copyClientHeaders(req, r, ad.StripHeaders())
	if err := ad.PrepareRequest(ctx, req, rt.Account, rt.KeyPlain, provider.SurfaceAnthropic); err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return 0, errCountUnavailable
	}
	var out struct {
		InputTokens int64 `json:"input_tokens"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.InputTokens <= 0 {
		return 0, errCountUnavailable
	}
	return out.InputTokens, nil
}

var errCountUnavailable = errors.New("proxy: upstream has no token count")

// parked reports whether rt is cooling down after a failure. A count must not
// knock on a hop the dispatcher is keeping traffic away from.
func (d *Dispatcher) parked(rt Route, now time.Time) bool {
	d.cooldownMu.Lock()
	defer d.cooldownMu.Unlock()
	st, ok := d.cooldowns[hopKey(rt)]
	return ok && st.until.After(now)
}
