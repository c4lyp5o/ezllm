package server

import (
	"github.com/c4lyp5o/ezllm/internal/jsonbody"
	"github.com/c4lyp5o/ezllm/internal/provider"
)

// Byte-entry conveniences for TESTS: production rides the request's shared
// single-parse decode (docs/single-parse-dispatch-design.md) through the
// *Doc forms directly, so nothing in the request path needs these. They stay
// here because the shape/hashing tests read better in byte form.

// extractModel reads the requested model without disturbing any other field.
// An absent/empty model returns ("", nil): the handler decides per surface
// whether that is a required-model refusal or a service-default lookup.
func extractModel(body []byte) (string, error) {
	return extractModelDoc(jsonbody.Open(body))
}

// captureShape decodes the body and derives the shape in one step.
func captureShape(body []byte, surface provider.Surface) shape {
	return captureShapeDoc(jsonbody.Open(body), surface)
}
