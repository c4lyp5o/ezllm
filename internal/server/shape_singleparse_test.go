package server

import (
	"testing"

	"github.com/c4lyp5o/ezllm/internal/jsonbody"
	"github.com/c4lyp5o/ezllm/internal/provider"
)

// captureShape now reads the request's single-parse decode
// (docs/single-parse-dispatch-design.md). Two contracts live here:

// TestCaptureShapeDocParityWithBytePath: for bodies WITHOUT number-literal
// edge cases, the doc path and the byte path must hash identically. The
// request path switched to captureShapeDoc on the shared decode; this guards
// the switch against silent drift in the hashing itself.
func TestCaptureShapeDocParityWithBytePath(t *testing.T) {
	bodies := []string{
		`{"model":"m","system":"s","tools":[{"type":"function","function":{"name":"f"}}],"messages":[{"role":"user","content":"one"},{"role":"assistant","content":"two"},{"role":"user","content":"three"}]}`,
		`{"model":"m","instructions":"be kind","input":[{"role":"user","content":"a"},{"role":"assistant","content":"b"}]}`,
		`{"model":"m","messages":[{"role":"user","content":"only one"}]}`,
		`{"model":"m"}`,
		`not json`,
		`{"model":"m","messages":[]}`,
	}
	for _, b := range bodies {
		legacy := captureShape([]byte(b), provider.SurfaceOpenAI)
		doc := captureShapeDoc(jsonbody.Open([]byte(b)), provider.SurfaceOpenAI)
		if legacy != doc {
			t.Errorf("shape drift for %s\nlegacy: %+v\ndoc:    %+v", b, legacy, doc)
		}
	}
}

// TestCaptureShapeDocNumberLiteralSensitivity is the INTENDED divergence from
// the old float64 decode: the hash now sees JSON number literals verbatim.
// `1e10` and `10000000000` are different effective prefixes (a tokenizer
// renders different tokens), so they MUST hash differently — the old float64
// normalization collapsed them and could hide a cache break. Big ints keep
// exact digits instead of being rounded through float64.
func TestCaptureShapeDocNumberLiteralSensitivity(t *testing.T) {
	// The literal must sit in the STABLE CORE (any turn but the newest —
	// prefix_sha deliberately drops the last message), or it never reaches
	// the hash at all.
	mk := func(num string) string {
		return `{"model":"m","messages":[{"role":"user","content":"` + num + `"},{"role":"assistant","content":"tail"}]}`
	}
	a := captureShapeDoc(jsonbody.Open([]byte(mk(`1e10`))), provider.SurfaceOpenAI)
	b := captureShapeDoc(jsonbody.Open([]byte(mk(`10000000000`))), provider.SurfaceOpenAI)
	if a.PrefixSHA == "" || a.PrefixSHA == b.PrefixSHA {
		t.Errorf("number-literal sensitivity lost: %q vs %q", a.PrefixSHA, b.PrefixSHA)
	}
}
