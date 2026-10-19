package jsonbody

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

// The hop body is the client's own bytes with only the model value changed:
// key order, whitespace, escapes and nested "model" keys all survive.
func TestSpliceModelKeepsClientBytes(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"swap keeps order and spacing",
			`{ "stream": true,  "model" : "old", "messages":[{"role":"user","content":"a \"model\": b"}] }`,
			`{ "stream": true,  "model" : "hop/m", "messages":[{"role":"user","content":"a \"model\": b"}] }`},
		{"nested model keys untouched",
			`{"tools":[{"model":"inner"}],"model":"old","meta":{"model":{"x":[1,{"model":2}]}}}`,
			`{"tools":[{"model":"inner"}],"model":"hop/m","meta":{"model":{"x":[1,{"model":2}]}}}`},
		{"escapes and html pass through",
			`{"model":"old","content":"<b>&é\u00e9\\\" ]}"}`,
			`{"model":"hop/m","content":"<b>&é\u00e9\\\" ]}"}`},
		{"null model replaced", `{"model":null,"n":1e10}`, `{"model":"hop/m","n":1e10}`},
		{"absent model inserted first", `{"messages":[]}`, `{"model":"hop/m","messages":[]}`},
		{"empty object", `{ }`, `{"model":"hop/m"}`},
		{"escaped key is the model key", `{"mod\u0065l":"old"}`, `{"mod\u0065l":"hop/m"}`},
		{"surrounding whitespace and trailing bytes dropped", " \n{\"model\":\"old\"}\n  junk", `{"model":"hop/m"}`},
		{"big number literal exact", `{"model":"m","max_tokens":12345678901234567890}`, `{"model":"hop/m","max_tokens":12345678901234567890}`},
	}
	for _, c := range cases {
		got, ok := SpliceModel([]byte(c.in), "hop/m")
		if !ok || string(got) != c.want {
			t.Errorf("%s:\n got %s (ok=%v)\nwant %s", c.name, got, ok, c.want)
		}
	}
}

// Where the splice would pass through something the re-marshal normalizes,
// it declines and the caller re-marshals.
func TestSpliceModelDeclines(t *testing.T) {
	for name, in := range map[string]string{
		"duplicate model key":           `{"model":"a","model":"b"}`,
		"duplicate stream key":          `{"model":"a","stream":false,"stream":true}`,
		"duplicate via escaped key":     `{"model":"a","mod\u0065l":"b"}`,
		"invalid utf-8 is re-sanitized": "{\"model\":\"a\",\"c\":\"\xff\"}",
	} {
		if _, ok := SpliceModel([]byte(in), "m"); ok {
			t.Errorf("%s: splice accepted %q, want fallback", name, in)
		}
	}
	// A nested duplicate carries no routing fact: passed through as sent.
	if _, ok := SpliceModel([]byte(`{"model":"a","messages":[{"role":"x","role":"y"}]}`), "m"); !ok {
		t.Error("nested duplicate keys should not force a re-marshal")
	}
}

// WithModel falls back to the re-marshal exactly where the splice declines,
// so a duplicate top-level key still collapses before it reaches a wire.
func TestWithModelFallsBackToMarshal(t *testing.T) {
	out, err := Open([]byte(`{"model":"a","stream":false,"stream":true}`)).WithModel("m")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"model":"m","stream":true}` {
		t.Errorf("fallback output = %s", out)
	}
}

// For every body the pipeline dispatches, the splice must decode to exactly
// the object the re-marshal would have sent: same keys, same values, same
// number literals. Only the byte layout may differ.
func FuzzSpliceMatchesMarshal(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add([]byte(s), "fuzz/model")
	}
	f.Add([]byte(`{"a":"\"}]","model":"x","b":[{"c":"{"}]}`), "n")
	f.Add([]byte(`{"model":"x"}`), "<&>\u2028")
	f.Fuzz(func(t *testing.T, body []byte, target string) {
		d := Open(body)
		if d.Err() != nil || d.Map() == nil {
			return
		}
		spliced, ok := SpliceModel(body, target)
		if !ok {
			return
		}
		d.m["model"] = target
		marshaled, err := json.Marshal(d.m)
		if err != nil {
			return
		}
		decode := func(b []byte) map[string]any {
			var m map[string]any
			dec := json.NewDecoder(bytes.NewReader(b))
			dec.UseNumber()
			if err := dec.Decode(&m); err != nil {
				t.Fatalf("output does not decode: %v\n%s", err, b)
			}
			if dec.More() {
				t.Fatalf("output has trailing data:\n%s", b)
			}
			return m
		}
		if a, b := decode(spliced), decode(marshaled); !reflect.DeepEqual(a, b) {
			t.Fatalf("splice drifted from re-marshal\nin:      %s\nspliced: %s\nmarshal: %s", body, spliced, marshaled)
		}
	})
}
