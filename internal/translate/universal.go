package translate

import (
	"encoding/json"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// Universal-cell conversions. The three NEW cross-wire pairs
// (chat→Messages, Responses→chat, Responses→Messages) ride the strict
// surface-neutral codecs; the legacy pairs keep their characterized wrappers
// (IsUniversalCell decides, so selection and codec choice cannot disagree).
// Everything here is strict: an unsupported request feature is a typed
// refusal, never a silent loss.

func decodeRequestFor(body []byte, client provider.Surface) (*RequestIR, error) {
	switch client {
	case provider.SurfaceChat:
		return DecodeChatRequest(body)
	case provider.SurfaceAnthropic:
		return DecodeMessagesRequest(body)
	case provider.SurfaceResponses:
		return DecodeResponsesRequest(body)
	}
	return nil, invalidResponse("unknown request surface " + string(client))
}

func encodeRequestFor(ir *RequestIR, upstream provider.Surface, model string) ([]byte, error) {
	switch upstream {
	case provider.SurfaceChat:
		return EncodeChatRequest(ir, model)
	case provider.SurfaceAnthropic:
		return EncodeMessagesRequest(ir, model)
	case provider.SurfaceResponses:
		return EncodeResponsesRequest(ir, model)
	}
	return nil, invalidResponse("unknown request destination " + string(upstream))
}

// RequestUniversal converts a client request onto the upstream's wire through
// the surface-neutral request IR.
func RequestUniversal(body []byte, client, upstream provider.Surface, model string) ([]byte, error) {
	ir, err := decodeRequestFor(body, client)
	if err != nil {
		return nil, err
	}
	return encodeRequestFor(ir, upstream, model)
}

// ResponseUniversal converts an upstream response back into the client's
// wire. When the upstream speaks Messages, opts.MessagesUsage must be the
// account's explicit convention — the router refuses the cell without one,
// so an unstated convention can never reach this decoder as a guess.
func ResponseUniversal(body []byte, upstream, client provider.Surface, model string, opts CodecOptions) ([]byte, error) {
	ir, err := DecodeResponse(body, upstream, opts)
	if err != nil {
		return nil, err
	}
	return EncodeResponse(ir, client, model, opts)
}

// ReorientError re-shapes an upstream error envelope into the CLIENT wire's
// error family: chat and Responses share {"error":{…}}, while the Messages
// wire wraps the same fields as {"type":"error","error":{…}}. Unparsable
// bodies pass through byte-identical — mangling an unknown shape helps
// nobody, and the ledger's err column wants the upstream's own words.
func ReorientError(body []byte, client provider.Surface) []byte {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return body
	}
	raw, ok := top["error"]
	if !ok || string(raw) == "null" {
		return body
	}
	var e ErrorIR
	if err := json.Unmarshal(raw, &e); err != nil {
		return body
	}
	return EncodeError(e, client)
}
