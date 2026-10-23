package jsonbody

import (
	"strings"
	"testing"
)

// The parity contract of this package is "exactly the decisions the old
// per-site decoders made" (docs/single-parse-dispatch-design.md). Each table
// below pins one decision class; a change here is a wire-behavior change and
// needs the design doc's review first.

func TestOpen(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		wantE  bool
		nilMap bool
	}{
		{"object", `{"model":"m"}`, false, false},
		{"empty", ``, true, true},
		{"array", `[]`, true, true},
		{"number", `123`, true, true},
		{"string", `"hi"`, true, true},
		{"literal null", `null`, false, true},
		{"trailing garbage", `{"model":"m"} junk`, true, true},
		{"trailing second value", `{"model":"m"}{"x":1}`, true, true},
		{"trailing space ok", `{"model":"m"}  `, false, false},
		// Decoder.More() reports false before a closing delimiter, so these
		// passed the old trailing-data check.
		{"trailing close bracket", `{"model":"m"}]`, true, true},
		{"trailing close brace", `{"model":"m"}}`, true, true},
	} {
		d := Open([]byte(tc.body))
		if (d.Err() != nil) != tc.wantE {
			t.Errorf("%s: Err()=%v, want error=%v", tc.name, d.Err(), tc.wantE)
		}
		if tc.nilMap != (d.Map() == nil) {
			t.Errorf("%s: Map()==nil is %v, want %v", tc.name, d.Map() == nil, tc.nilMap)
		}
	}
}

func TestModelParity(t *testing.T) {
	// extractModel's old struct-decode decision tree, case by case. The error
	// STRING is part of the contract (the client sees it verbatim in a 400).
	for _, tc := range []struct {
		name  string
		body  string
		want  string
		wantE bool
	}{
		{"absent", `{"stream":true}`, "", false},
		{"null", `{"model":null}`, "", false},
		{"string", `{"model":"ns/m"}`, "ns/m", false},
		{"whitespace", `{"model":"  ns/m\t"}`, "ns/m", false},
		{"empty string", `{"model":""}`, "", false},
		{"number", `{"model":123}`, "", true},
		{"bool", `{"model":true}`, "", true},
		{"object", `{"model":{"a":1}}`, "", true},
		{"array", `{"model":["m"]}`, "", true},
		{"bad json", `{"model":`, "", true},
		{"trailing junk", `{"model":"m"} x`, "", true},
	} {
		model, _, err := Open([]byte(tc.body)).Model()
		if (err != nil) != tc.wantE {
			t.Errorf("%s: err=%v, want error=%v", tc.name, err, tc.wantE)
		}
		if err != nil && err.Error() != "body is not a JSON object" {
			t.Errorf("%s: error string %q drifted from the old client-visible one", tc.name, err)
		}
		if model != tc.want {
			t.Errorf("%s: model=%q, want %q", tc.name, model, tc.want)
		}
	}
}

func TestStreamParity(t *testing.T) {
	// IsStream's old struct-decode decision tree: only a real JSON true
	// streams — `"true"`, `1`, `"yes"`, garbage all mean non-streaming.
	for _, tc := range []struct {
		body string
		want bool
	}{
		{`{"stream":true}`, true},
		{`{"stream":false}`, false},
		{`{}`, false},
		{`{"stream":"true"}`, false},
		{`{"stream":1}`, false},
		{`{"stream":null}`, false},
		{`junk`, false},
	} {
		if got := Open([]byte(tc.body)).Stream(); got != tc.want {
			t.Errorf("Stream(%s) = %v, want %v", tc.body, got, tc.want)
		}
	}
}

func TestWithModel(t *testing.T) {
	// WithModel is the old SwapModel map round-trip minus its decode: the
	// model changes, EVERYTHING else survives (unknown provider fields,
	// UseNumber literals, nested structures).
	d := Open([]byte(`{"model":"old","unknown":{"deep":[1,2,{"x":"y"}]},"thinking":{"type":"enabled"},"max_tokens":12345678901234567890,"stream":true,"messages":[{"role":"user","content":"héllo 🌈"}]}`))
	out, err := d.WithModel("new-ns/new-model")
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		`"model":"new-ns/new-model"`,
		`"unknown":{"deep":[1,2,{"x":"y"}]}`,
		`"thinking":{"type":"enabled"}`,
		// UseNumber fidelity: the big int must appear as its EXACT literal,
		// never as a float64 rendering like 1.2345678901234567e+19.
		`12345678901234567890`,
		`"stream":true`,
		"héllo 🌈",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("WithModel output missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "1.2345678901234567e+19") {
		t.Errorf("big int was mangled by float64:\n%s", s)
	}

	// Reuse across hops: the doc keeps serving later swaps (hops are serial).
	out2, err := d.WithModel("hop-2")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out2), `"model":"hop-2"`) || !strings.Contains(string(out2), `"thinking":`) {
		t.Errorf("second WithModel lost fields:\n%s", out2)
	}
}

func TestWithModelRefusesNonObject(t *testing.T) {
	// parity with SwapModel's explicit nil-map refusal
	for _, body := range []string{`null`, ``, `[]`} {
		if _, err := Open([]byte(body)).WithModel("m"); err == nil {
			t.Errorf("WithModel(%s) should refuse", body)
		}
	}
}
