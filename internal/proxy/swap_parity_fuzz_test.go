package proxy

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/jsonbody"
)

// FuzzSwapParity is the single-parse wire-fidelity moat
// (docs/single-parse-dispatch-design.md). The invariant is PIPELINE parity,
// not component parity: for every body the OLD pipeline would have
// DISPATCHED, the new path must emit byte-identical hop bodies.
//
// The gate is a re-implementation of the old request path's FIRST decision —
// extractModel's struct Unmarshal — because that is where the old pipeline
// rejected bodies before any hop body was ever built. Legacy SwapModel is
// LAX on its own (it tolerates trailing garbage: dec.Decode stops after the
// first value), but that laxness was unreachable in the old pipeline: a
// trailing-junk body died inside extractModel with a 400 and never reached a
// hop. The new pipeline reaches the same verdict through jsonbody's
// trailing-data rejection. Bodies the old gate rejected are therefore
// excluded from the parity comparison — on them, both architectures answer
// 400 before dispatch, which is the behavior that matters.
func FuzzSwapParity(f *testing.F) {
	for _, fx := range swapParityFixtures {
		f.Add([]byte(fx.body), "hop/model")
	}
	f.Add([]byte(`{"model":"m","stream":true,"messages":[]}`), "ns/other")
	f.Add([]byte(`{"model":null,"n":1e10}`), "x")
	f.Add([]byte(`{"model":"m","model":"second"}`), "x")
	f.Add([]byte(`{}`), "x")
	f.Add([]byte(`null`), "x")
	f.Add([]byte(`[]`), "x")
	f.Add([]byte(`{"model":"m"} junk`), "x") // old extractModel rejected this one

	f.Fuzz(func(t *testing.T, body []byte, target string) {
		// The old pipeline's gate, verbatim (pre-single-parse extractModel).
		var m struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &m); err != nil {
			return // the old pipeline answered 400 here; nothing dispatched
		}

		legacyOut, _, legacyErr := SwapModel(body, target)
		docOut, docErr := jsonbody.Open(body).WithModel(target)

		if (legacyErr == nil) != (docErr == nil) {
			t.Fatalf("refusal mismatch: legacy err=%v doc err=%v\nbody: %s", legacyErr, docErr, body)
		}
		if legacyErr != nil {
			return // both refused — parity holds
		}
		if !bytes.Equal(legacyOut, docOut) {
			t.Fatalf("wire drift!\nlegacy: %s\ndoc:    %s", legacyOut, docOut)
		}
	})
}

// TestSwapParityTrailingGarbageGate pins the subtlety the fuzz found, so it
// can never silently regress: a trailing-junk body is a 400 in BOTH
// architectures — old extractModel rejected it, new jsonbody.Open rejects
// it — even though legacy SwapModel standing alone would have tolerated it.
func TestSwapParityTrailingGarbageGate(t *testing.T) {
	body := []byte(`{"model":"m"} junk`)

	// Old gate: rejected.
	var m struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &m); err == nil {
		t.Fatal("old extractModel should reject trailing junk — the oracle changed")
	}
	// New gate: rejected.
	if _, _, err := jsonbody.Open(body).Model(); err == nil {
		t.Fatal("jsonbody must reject trailing junk (extractModel parity)")
	}
	// Legacy SwapModel standing alone tolerates it — document the fact so the
	// laxity is understood as unreachable-in-pipeline, not as a porting bug.
	if _, _, err := SwapModel(body, "hop/model"); err != nil {
		t.Fatalf("SwapModel tolance changed unexpectedly: %v", err)
	}
}
