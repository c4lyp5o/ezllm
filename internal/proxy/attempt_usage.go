package proxy

import (
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/c4lyp5o/ezllm/internal/store"
)

// discardAttempt records only observed usage on an abandoned response. It
// never invents charges for an error that has no usage, and never folds one
// account's spend into another account's delivered result.
func discardAttempt(resp *http.Response, rt Route, res Result, start time.Time, led *attemptLedger) {
	if resp == nil {
		return
	}
	defer resp.Body.Close()
	var usage *store.Usage
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		tap := NewUsageTapperFor(rt.EffectiveWire(), rt.Account.UsageConvention)
		reader := &sourceUsageReader{ReadCloser: resp.Body, tap: tap}
		io.Copy(io.Discard, io.LimitReader(reader, maxBodyBytes))
		usage = tap.Usage()
	} else {
		raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
		if err == nil && len(raw) <= maxBodyBytes {
			usage = tapJSONUsage(raw, rt.EffectiveWire(), rt.Account.UsageConvention)
		}
	}
	if usage == nil || led == nil {
		return
	}
	led.spent = append(led.spent, store.Call{
		TS: start, AccountID: rt.Account.ID, Account: rt.Account.Namespace, KeyID: rt.KeyID, KeyHint: rt.KeyHint,
		Model: rt.Model, Alias: rt.Alias, Surface: usageSurface(rt.Surface), Status: res.Status, Stream: res.Stream,
		Totalms: msPtr(time.Since(start)), EndpointID: res.EndpointID, UpstreamModel: res.UpstreamModel,
		TokensIn: usage.In, TokensOut: usage.Out, TokensCachedRead: usage.CachedRead, TokensCachedWrite: usage.CachedWrite, ReasoningTokens: usage.Reasoning, RawUsage: store.RawJSON(usage.Raw),
		Translated: rt.EffectiveWire() != rt.Surface, Err: "upstream attempt discarded before delivery",
	})
}
