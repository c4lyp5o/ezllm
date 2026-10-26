// Package jsonbody parses a request body exactly once per request so routing,
// compression, shape capture, and the per-hop model swap all share one decode.
//
// Why: M1 measured that the hot path JSON-decoded the same body up to five
// times (the old extractModel, IsStream, SwapModel-per-hop, compress.Apply and
// captureShape call sites), and the parse cost is what makes
// inspection+routing scale with payload (~1.5 ms/MiB against ~1 MB dominant
// traffic). BodyDoc is that one decode.
//
// Parity with the code it replaces is the contract, documented field by field
// in docs/single-parse-dispatch-design.md:
//
//   - Same decoder settings as SwapModel: json.Decoder + UseNumber, so large
//     ints stay exact (no float64 mangling).
//   - Trailing garbage rejected: a bare json.Unmarshal accepts the first
//     value and ignores the rest, while dec.Decode + dec.More check mirrors
//     the old extractModel/SwapModel fail-closed behavior exactly.
//   - Model/Stream accessors carry the OLD semantics: null and absent mean
//     "no value"; a non-string model or a non-true stream is an error/false,
//     exactly what the old struct decodes did.
//   - WithModel splices the new model into the original bytes (SpliceModel),
//     falling back to json.Marshal(map) where a splice cannot normalize the
//     body; SwapModel runs the same two steps, so both paths stay
//     byte-identical for the same input.
//
// Zero external dependencies (stdlib encoding/json only) — the no-CGO static
// image constraint stays intact.
package jsonbody

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// BodyDoc is one decoded request body.
//
// A BodyDoc is immutable except for WithModel, which is only safe when
// requests touch their own doc (one per request, hops are tried serially by
// ForwardCandidates). Map() exposes the decoded map for the same reason —
// consumers that need it (compression, shape capture) must treat it read-only.
type BodyDoc struct {
	raw []byte
	m   map[string]any
	err error
}

// Open decodes body with the same settings and fail-closed semantics the
// legacy per-site decoders used. Never panics; a malformed body yields a doc
// whose Err() is non-nil (and whose Map() is nil).
func Open(body []byte) *BodyDoc {
	d := &BodyDoc{raw: body}
	if len(body) == 0 {
		d.err = errors.New("jsonbody: empty body")
		return d
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber() // avoid float64 mangling of large ints (SwapModel parity)
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		d.err = fmt.Errorf("jsonbody: body is not a JSON object: %w", err)
		return d
	}
	// A second value after the object means trailing garbage. Both legacy
	// sites (struct Unmarshal in the old extractModel/IsStream, Decoder in
	// SwapModel) rejected that, so we must too — and dec.Decode alone does
	// NOT. Only EOF may follow: More() reports false before a stray closing
	// delimiter.
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		d.err = errors.New("jsonbody: trailing data after JSON object")
		return d
	}
	d.m = m // nil for a literal `null` body — consumers must handle Map()==nil
	return d
}

// Err reports the decode failure, if any.
func (d *BodyDoc) Err() error { return d.err }

// Bytes returns the original body bytes, untouched.
func (d *BodyDoc) Bytes() []byte { return d.raw }

// Map returns the decoded object, nil-safe for reads (nil map reads return
// zero). Callers MUST NOT mutate it; WithModel is the only writer.
func (d *BodyDoc) Map() map[string]any { return d.m }

// Model is the old byte-entry extractModel decision tree over the decoded
// body (server.go reaches it through extractModelDoc):
// absent or null → ("", false, nil); a real string → (trimmed, true, nil);
// a non-string value (number, bool, object, array) or a decode failure →
// error "body is not a JSON object", matching the old wrapped error string.
func (d *BodyDoc) Model() (string, bool, error) {
	if d.err != nil {
		return "", false, errors.New("body is not a JSON object")
	}
	v, ok := d.m["model"]
	if !ok || v == nil {
		return "", false, nil
	}
	s, ok := v.(string)
	if !ok {
		return "", false, errors.New("body is not a JSON object")
	}
	return strings.TrimSpace(s), true, nil
}

// Stream is IsStream's exact decision tree: only a real JSON true streams.
func (d *BodyDoc) Stream() bool {
	if d.err != nil {
		return false
	}
	b, ok := d.m["stream"].(bool)
	return ok && b
}

// WithModel returns body bytes for the hop: the client's bytes with `model`
// set to newModel (SpliceModel), or, where a splice cannot be used, the
// decoded map re-marshaled (key-sorted, unknown fields preserved, UseNumber
// literals exact). SwapModel makes the same choice, so the two never differ.
// The receiver's map keeps the value for the next hop: hops are tried
// serially and the last write is the only one that matters.
func (d *BodyDoc) WithModel(newModel string) ([]byte, error) {
	if d.err != nil {
		return nil, errors.New("proxy: body is not a JSON object")
	}
	if d.m == nil {
		// a literal `null` body: decode succeeded but there is no object to
		// rewrite — same refusal SwapModel gives today.
		return nil, errors.New("proxy: body is not a JSON object")
	}
	d.m["model"] = newModel
	if out, ok := SpliceModel(d.raw, newModel); ok {
		return out, nil
	}
	out, err := json.Marshal(d.m)
	if err != nil {
		return nil, fmt.Errorf("proxy: re-marshal: %w", err)
	}
	return out, nil
}
