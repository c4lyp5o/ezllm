package server

import (
	"testing"

	"github.com/c4lyp5o/ezllm/internal/jsonbody"
	"github.com/c4lyp5o/ezllm/internal/provider"
)

// captureShapeDoc reads the request's single-parse decode
// (docs/single-parse-dispatch-design.md).

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
	a := captureShapeDoc(jsonbody.Open([]byte(mk(`1e10`))), provider.SurfaceChat)
	b := captureShapeDoc(jsonbody.Open([]byte(mk(`10000000000`))), provider.SurfaceChat)
	if a.PrefixSHA == "" || a.PrefixSHA == b.PrefixSHA {
		t.Errorf("number-literal sensitivity lost: %q vs %q", a.PrefixSHA, b.PrefixSHA)
	}
}
