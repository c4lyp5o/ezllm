package proxy

import (
	"bytes"
	"encoding/json"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// clampReasoning rewrites the client's `reasoning_effort` for ONE hop, using
// the vocabulary that model itself named in a previous refusal.
//
// It returns the original slice untouched — not a copy — whenever nothing
// changes, which is the common case: an unknown rule, a body without the field,
// or a value the model already accepts. Only a request that would have been
// refused is ever re-encoded.
//
// Scope, deliberately: the top-level `reasoning_effort` string of an
// OpenAI-shaped body. That is the field every provider in Calypso's ledger
// rejected, and the one both the native path and the chat→responses translator
// read. A nested `reasoning.effort` (Responses) or anthropic's `thinking` block
// is a different vocabulary and is not touched here — guessing would be worse
// than leaving it alone.
func clampReasoning(body []byte, rule provider.ReasoningRule) (out []byte, changed bool) {
	if !rule.Known {
		return body, false
	}
	// JSON permits whitespace before the opening object. Inspect a view while
	// retaining the original slice for every unchanged request.
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return body, false
	}
	var doc map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber() // literals stay exact through the round trip
	if err := dec.Decode(&doc); err != nil || doc == nil {
		return body, false
	}
	raw, ok := doc["reasoning_effort"]
	if !ok {
		return body, false
	}
	want, ok := raw.(string)
	if !ok {
		return body, false // a non-string is the validator's problem, not ours
	}
	value, drop, changed := rule.Clamp(want)
	if !changed {
		return body, false
	}
	if drop {
		delete(doc, "reasoning_effort")
	} else {
		doc["reasoning_effort"] = value
	}
	encoded, err := json.Marshal(doc)
	if err != nil {
		return body, false // never send a half-rewritten body
	}
	return encoded, true
}
