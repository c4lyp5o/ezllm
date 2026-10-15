package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/jsonbody"
	"github.com/c4lyp5o/ezllm/internal/provider"
)

// Single-parse dispatch (docs/single-parse-dispatch-design.md) swaps a hop's
// model by mutating the REQUEST's one shared decode instead of re-decoding
// per hop. The acceptance bar is byte equality: for every body class the old
// SwapModel handles, the doc path must emit the EXACT bytes SwapModel emits.
// If this test ever fails, the wire changed — stop and read the design doc.

var swapParityFixtures = []struct {
	name string
	body string
}{
	{
		"unknown provider fields survive",
		`{"model":"client/model","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled"},"extra_body":{"x":1},"previous_response_id":"pr_123","vendor_ext":[{"a":[1,2]}]}`,
	},
	{
		"big ints survive as literals",
		`{"model":"m","messages":[],"max_tokens":12345678901234567890,"seed":9007199254740993}`,
	},
	{
		"unicode and escapes survive",
		`{"model":"m","messages":[{"role":"user","content":"héllo 🌈 \u0041 line\nend"}],"n":1}`,
	},
	{
		"nested tool definitions survive",
		`{"model":"m","tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}},{"type":"function","function":{"name":"g","parameters":{}}}],"tool_choice":"auto","messages":[]}`,
	},
	{
		"duplicate keys collapse to last-wins everywhere",
		`{"model":"first","model":"second","stream":true,"stream":true}`,
	},
	{
		"streaming flag and bare fields",
		`{"model":"m","stream":true,"temperature":0.7,"top_p":1e10,"messages":[]}`,
	},
	{
		"null model stays an object with model replaced",
		`{"model":null,"messages":[{"role":"user","content":"x"}]}`,
	},
}

// TestSwapModelByteIdenticalToDocPath is the acceptance gate: legacy
// SwapModel output and the jsonbody.WithModel output must be byte-equal.
func TestSwapModelByteIdenticalToDocPath(t *testing.T) {
	for _, fx := range swapParityFixtures {
		legacy, _, err := SwapModel([]byte(fx.body), "hop/model")
		if err != nil {
			t.Fatalf("%s: legacy SwapModel: %v", fx.name, err)
		}
		got, err := jsonbody.Open([]byte(fx.body)).WithModel("hop/model")
		if err != nil {
			t.Fatalf("%s: doc WithModel: %v", fx.name, err)
		}
		if !bytes.Equal(legacy, got) {
			t.Errorf("%s: doc path emitted DIFFERENT bytes than SwapModel\nlegacy: %s\ndoc:    %s", fx.name, legacy, got)
		}
	}
}

// captureUpstream records the body an upstream stub actually received.
type captureUpstream struct {
	mu   sync.Mutex
	body []byte
}

func (c *captureUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	c.body = b
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"ok":true}`)
}

// TestForwardCandidatesDocPathByteIdentical proves the WHOLE dispatch emits
// identical upstream bytes with and without the shared decode — the doc is a
// pure optimization hint, not a behavior fork. Fails if WithModel mutates
// anything but `model` (e.g. drops a field, reorders semantics, mangles a
// number), because the two runs must agree byte-for-byte.
func TestForwardCandidatesDocPathByteIdentical(t *testing.T) {
	for _, fx := range swapParityFixtures {
		var gotLegacy, gotDoc []byte
		for _, withDoc := range []bool{false, true} {
			up := &captureUpstream{}
			srv := httptest.NewServer(up)
			defer srv.Close()
			d := newDispatcher()
			rt := textRoute(srv.URL, "hop/model")
			body := []byte(fx.body)
			req, _ := reqWithBody(fx.body)
			var err error
			if withDoc {
				_, _, err = d.ForwardCandidatesDoc(context.Background(), httptest.NewRecorder(), req,
					[]Route{rt}, body, jsonbody.Open(body))
			} else {
				_, _, err = d.ForwardCandidates(context.Background(), httptest.NewRecorder(), req,
					[]Route{rt}, body)
			}
			if err != nil {
				t.Fatalf("%s (doc=%v): dispatch: %v", fx.name, withDoc, err)
			}
			up.mu.Lock()
			if withDoc {
				gotDoc = append([]byte(nil), up.body...)
			} else {
				gotLegacy = append([]byte(nil), up.body...)
			}
			up.mu.Unlock()
		}
		if !bytes.Equal(gotLegacy, gotDoc) {
			t.Errorf("%s: upstream saw DIFFERENT bytes across paths\nlegacy: %s\ndoc:    %s", fx.name, gotLegacy, gotDoc)
		}
		if !bytes.Contains(gotDoc, []byte(`"model":"hop/model"`)) {
			t.Errorf("%s: model was not swapped:\n%s", fx.name, gotDoc)
		}
	}
}

// TestDocPathStreamFlag is the request-level stream flag the ledger records:
// a doc-backed dispatch must report Stream exactly as the old per-attempt
// parse did, including when the hop body was rewritten around it.
func TestDocPathStreamFlag(t *testing.T) {
	up := &captureUpstream{}
	srv := httptest.NewServer(up)
	defer srv.Close()
	d := newDispatcher()
	rt := textRoute(srv.URL, "hop/model")
	body := []byte(`{"model":"m","stream":true,"messages":[]}`)
	req, _ := reqWithBody(string(body))
	_, res, err := d.ForwardCandidatesDoc(context.Background(), httptest.NewRecorder(), req,
		[]Route{rt}, body, jsonbody.Open(body))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Stream {
		t.Error("doc path lost the streaming flag")
	}

	// and a NON-streaming request must not gain one
	body2 := []byte(`{"model":"m","messages":[]}`)
	req2, _ := reqWithBody(string(body2))
	_, res2, err := d.ForwardCandidatesDoc(context.Background(), httptest.NewRecorder(), req2,
		[]Route{rt}, body2, jsonbody.Open(body2))
	if err != nil {
		t.Fatal(err)
	}
	if res2.Stream {
		t.Error("doc path invented a streaming flag")
	}
	_ = provider.SurfaceOpenAI // keep the import for the Route construction helpers
}
