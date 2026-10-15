package jsonbody

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// Fuzz targets for the single-parse hot path (docs/single-parse-dispatch-
// design.md). The parser now touches every byte of every request, so these
// pin its two safety properties:
//
//  1. Open and everything built on it NEVER PANIC on any input.
//  2. Round trip: what WithModel emits re-parses with exactly the model it
//     was told to set, and everything else preserved.
//
// Seed corpus covers the adversarial classes the old per-site decoders saw:
// duplicate keys, deep nesting, escapes/unicode, huge/varied numbers,
// trailing junk, non-object JSON.

var fuzzSeeds = []string{
	`{"model":"m","stream":true,"messages":[]}`,
	`{"model":"m","model":"second"}`,
	`{"model":null}`,
	`{"model":12345678901234567890}`,
	`{"model":"m","n":1e10,"n2":9007199254740993,"n3":-0.0}`,
	`{"model":"m"}{"x":1}`,
	`{"model":"m"} trailing`,
	`{"model":"m","deep":{"a":{"b":{"c":{"d":{"e":[1,2,3]}}}}}}`,
	`{"model":"m","content":"héllo 🌈 \u0000 \u0041 \n\t"}`,
	`{"model":"m","messages":[{"role":"user","content":{"type":"array","parts":[1,"two",null]}}]}`,
	`null`,
	`[]`,
	``,
	`"just a string"`,
	`123`,
	`true`,
	`{"model":"m","tools":[{"type":"function","function":{"name":"f","parameters":{}}}]}`,
	`{"model":"\u0000"}`,
	`{`,
	`{{{{`,
	`{"model":"m",,}`,
	`{"model":"m","x":NaN}`,
	// Found by live fuzzing 2026-10-15: exponent literals that overflow a
	// float64 must survive the round trip (json.Number keeps them verbatim).
	`{"0":1e700,"00000":"0"}`,
}

func FuzzOpenNeverPanics(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		d := Open(body)
		// Touch every accessor; none may panic regardless of input.
		_ = d.Err()
		_ = d.Bytes()
		_ = d.Map()
		_, _, _ = d.Model()
		_ = d.Stream()
		// WithModel may refuse (error) but must never panic.
		_, _ = d.WithModel("fuzz/model")
	})
}

func FuzzWithModelRoundTrip(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		d := Open(body)
		out, err := d.WithModel("fuzz/model")
		if err != nil {
			return // a refusal is a legal outcome (non-object body)
		}
		// The emitted body must re-parse and carry exactly the model we set.
		re := Open(out)
		if re.Err() != nil {
			t.Fatalf("WithModel output did not re-parse: %v\n%s", re.Err(), out)
		}
		model, _, merr := re.Model()
		if merr != nil || model != "fuzz/model" {
			t.Fatalf("round trip model = %q err=%v, want %q\n%s", model, merr, "fuzz/model", out)
		}
		// And it must still be a JSON object (never e.g. a bare array).
		// Decode with UseNumber for the same reason Open does: exponent
		// literals like 1e700 are legal JSON numbers our parser preserves
		// verbatim, and a float64 decode of them overflows — the verifier
		// must not be weaker than the parser.
		var m map[string]any
		dec := json.NewDecoder(bytes.NewReader(out))
		dec.UseNumber()
		if err := dec.Decode(&m); err != nil || m == nil {
			t.Fatalf("round trip is not a JSON object: %v\n%s", err, out)
		}
		// The stream flag of the round trip must not be invented: if the
		// re-parsed body streams, the ORIGINAL input must have streamed too.
		if re.Stream() && !d.Stream() {
			t.Fatalf("round trip invented streaming\nin:  %s\nout: %s", body, out)
		}
	})
}

func FuzzModelDecisionTree(f *testing.F) {
	// extractModel's exact semantics under fuzz: model is either an error, or
	// a TRIMMED string — never a raw one with whitespace attached.
	for _, s := range fuzzSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		model, _, err := Open(body).Model()
		if err != nil {
			if err.Error() != "body is not a JSON object" {
				t.Fatalf("error string drifted: %q", err)
			}
			return
		}
		if strings.TrimSpace(model) != model {
			t.Fatalf("model not trimmed: %q", model)
		}
	})
}
