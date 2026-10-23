package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// canonEncoder must be byte-identical to json.Marshal for every value the
// shared decoder produces: prefix_sha values already in the ledger were
// hashed from json.Marshal output, and a drift would split every sibling
// group at the deploy.
func canonOf(t testing.TB, v any) []byte {
	t.Helper()
	var buf bytes.Buffer
	e := canonEncoder{w: &buf}
	if !e.encode(v) {
		t.Fatalf("canonEncoder refused %#v", v)
	}
	return buf.Bytes()
}

func decodeNumber(t testing.TB, doc string) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader([]byte(doc)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode %q: %v", doc, err)
	}
	return v
}

func TestCanonicalMatchesMarshal(t *testing.T) {
	for _, doc := range []string{
		`null`, `true`, `false`, `0`, `-0`, `1e10`, `12345678901234567890`, `3.14159`, `-2.5E-3`,
		`""`, `"plain"`, `"quote \" backslash \\ slash /"`,
		`"html <b> & </b>"`, `"ctl \u0000\u0001\u001f\b\f\n\r\t"`, `"del \u007f"`,
		`"line sep   para sep  "`, `"emoji 😀 and é and 中文"`, `"\ud800 lone surrogate"`,
		`[]`, `{}`, `[1,"a",null,true,[],{}]`,
		`{"b":1,"a":2,"A":3,"é":4,"_":5,"10":6,"9":7}`,
		`{"messages":[{"role":"user","content":"hi <there>"},{"role":"assistant","content":[{"type":"text","text":"x\ny"}]}],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}]}`,
	} {
		v := decodeNumber(t, doc)
		want, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if got := canonOf(t, v); !bytes.Equal(got, want) {
			t.Errorf("doc %s\n got %s\nwant %s", doc, got, want)
		}
	}
	// Invalid UTF-8 can arrive inside a decoded string only via raw bytes.
	raw := map[string]any{"bad\xffkey": "bad\xc3\x28value"}
	want, _ := json.Marshal(raw)
	if got := canonOf(t, raw); !bytes.Equal(got, want) {
		t.Errorf("invalid utf-8\n got %s\nwant %s", got, want)
	}
}

func FuzzCanonicalMatchesMarshal(f *testing.F) {
	for _, s := range []string{`{"a":"<x>"}`, `[1,2e5," "]`, `{"z":{"y":[null,true]}}`, `"\u0007"`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		var v any
		if dec.Decode(&v) != nil {
			return
		}
		want, err := json.Marshal(v)
		if err != nil {
			return
		}
		if got := canonOf(t, v); !bytes.Equal(got, want) {
			t.Fatalf("input %q\n got %s\nwant %s", data, got, want)
		}
	})
}

// The streamed prefix hash equals the pre-streaming computation,
// sha256(json.Marshal(prefix)), so prefix_sha values already in the ledger
// keep grouping with new ones.
func TestPrefixSHAUnchangedByStreaming(t *testing.T) {
	body := []byte(`{"model":"m","system":"be <brief> & exact","tools":[{"name":"f","input_schema":{"type":"object"}}],` +
		`"messages":[{"role":"user","content":"q \"1\"\n\tx"},{"role":"assistant","content":[{"type":"text","text":"a b"}]},{"role":"user","content":"now"}]}`)
	var root map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		t.Fatal(err)
	}
	msgs := root["messages"].([]any)
	legacy, _ := json.Marshal(map[string]any{"system": root["system"], "tools": root["tools"], "messages": msgs[:len(msgs)-1]})
	sum := sha256.Sum256(legacy)
	want := hex.EncodeToString(sum[:8])
	if got := captureShape(body, provider.SurfaceAnthropic).PrefixSHA; got != want {
		t.Fatalf("prefix_sha = %s, want the legacy %s", got, want)
	}
}
