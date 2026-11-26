package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
)

// retryableUpstreamStatus reports whether an upstream answer is worth retrying
// on the SAME hop.
//
// 429 is deliberately absent. A rate limit is not a transient glitch: the
// upstream has just told us this credential has no budget left in the current
// window, so an immediate retry spends MORE of the very budget being reported
// and guarantees the next answer is another 429. Retrying a rate-limited key is
// what turns "one call over budget" into "the whole minute gone".
//
// 429 is still a FAILOVER signal — see failoverStatus, which includes it, so a
// different hop with budget can serve the request. Retry and failover are
// different questions and must not share a predicate.
func retryableUpstreamStatus(code int) bool {
	return code == http.StatusRequestTimeout || code == http.StatusTooEarly || code >= 500
}

func retryableUpstreamError(err error) bool {
	var netErr net.Error
	return errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr)
}

// failoverStatus reports whether an upstream answer means "that hop couldn't
// serve it, another one might" rather than "the request itself is wrong".
//
// 400/413/422 are deliberately excluded: they describe the BODY, which we are
// about to replay byte-identical to the next hop — retrying would burn every
// key in the combo to get the same rejection. 401/403/404/429 and 5xx are
// hop-specific (bad key, model absent on that account, quota, outage) and are
// exactly what failover exists to survive.
func failoverStatus(code int) bool {
	switch code {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
		http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return code >= 500
}

// refusesWire reports a 4xx whose body says the hop cannot serve this wire
// at all (opencode-go's ModelProtocolUnsupported on /v1/messages). Unlike an
// ordinary 400 it describes the hop, not the body, so another hop can serve
// the identical request. The peeked prefix is restored, so the response stays
// committable as the final fallback.
func refusesWire(resp *http.Response) bool {
	if resp == nil || resp.Body == nil || resp.StatusCode != http.StatusBadRequest {
		return false
	}
	// Only the vendor's explicit capability words count: ErrorCause also maps
	// "does not allow" here, which on a 400 can just as well describe a body.
	s := strings.ToLower(string(peekBody(resp, maxErrPeek)))
	return strings.Contains(s, "modelprotocolunsupported") ||
		strings.Contains(s, "not supported for format") ||
		strings.Contains(s, "unsupported_api_for_model")
}

// refusesHistory reports a 400 whose body says the hop cannot serve this
// CONVERSATION: a thinking model demanding the reasoning of every earlier
// assistant turn, in a history another model produced. Like refusesWire it is a
// fact about the hop, not our body — the identical bytes are fine for a hop of
// the model's own family — so the dispatcher fails over instead of committing a
// 400 that ends a client run.
//
// It is deliberately NOT a park: the hop is healthy for conversations it started
// itself, and the refusal says nothing about its availability.
func refusesHistory(resp *http.Response) bool {
	if resp == nil || resp.Body == nil || resp.StatusCode != http.StatusBadRequest {
		return false
	}
	s := strings.ToLower(string(peekBody(resp, maxErrPeek)))
	return strings.Contains(s, "must be passed back to the api") ||
		strings.Contains(s, "needs the reasoning of every")
}

// refusesTools reports a 400 from a hop that cannot honor requested tools or
// parameters: the Copilot CLI bridge refuses `tools` by name (code
// chat_tools_unsupported) when the CLI's tool channel is missing or at another
// protocol version, or a tool cannot be registered. It describes the HOP, not the body, so a
// hop that does support tools can serve the identical request — committing the
// 400 instead ended the client run with three working hops left in the combo
// (measured 2026-11-05).
//
// Like refusesHistory it is NOT a park: the hop can serve other requests, and
// the refusal says nothing about its availability.
func refusesTools(resp *http.Response) bool {
	if resp == nil || resp.Body == nil || resp.StatusCode != http.StatusBadRequest {
		return false
	}
	s := strings.ToLower(string(peekBody(resp, maxErrPeek)))
	return strings.Contains(s, "chat_tools_unsupported") || strings.Contains(s, "chat_parameter_unsupported")
}

// deniesModelAccess reports a 401/403 whose body says the HOP cannot reach the
// model it was asked for ("Access to model denied", "model access denied",
// "does not have access to"). Measured 2026-10-23: an account answered 403 for
// one model, was re-dialled on all 12 remaining requests of its block, and
// every one of them fell over to a hop whose prompt cache hit 27% instead of
// 98% — the wasted dial is cheap, the cache it forces is not.
//
// Deliberately narrow: the words must name MODEL access. A 403 that describes
// the request (a policy on the body, a bad tool shape) keeps its old treatment
// — fail over, leave hop health alone — because parking a healthy hop on a
// caller's mistake is worse than one wasted round trip. The park is keyed by
// account+base+model (hopKey), so only that one model stops being dialled.
func deniesModelAccess(resp *http.Response) bool {
	if resp == nil || resp.Body == nil {
		return false
	}
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		return false
	}
	body := strings.ToLower(string(peekBody(resp, maxErrPeek)))
	for _, phrase := range []string{
		"access to model denied",
		"model access denied",
		"does not have access to",
		"no access to model",
	} {
		if strings.Contains(body, phrase) {
			return true
		}
	}
	return false
}

// peekBody reads up to n bytes of resp's body and puts them back in front of
// the rest, so the response stays committable byte-for-byte. The restored
// body still closes the ORIGINAL one: wrapping it in io.NopCloser (as both
// peekers used to) dropped Close, and a response abandoned before EOF then
// held its upstream connection.
func peekBody(resp *http.Response, n int) []byte {
	head := make([]byte, n)
	k, _ := io.ReadFull(resp.Body, head) // a short body yields ErrUnexpectedEOF; k holds what there was
	if k > 0 {
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(head[:k]), resp.Body), resp.Body}
	}
	return head[:k]
}
