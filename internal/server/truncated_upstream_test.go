package server

// End-to-end regression for the empty-200 defect.
//
// An upstream that answers with headers + a partial body and then hangs up
// makes commit() fail on the body read while carrying the upstream's 200.
// The server's error branch guarded that 502 on `res.Status == 0`, so the
// client was handed "200 OK" with a completely empty body — the exact outcome
// the guard's comment claims to prevent. The gate is now `!res.Committed`.

import (
	"net/http"
	"strings"
	"testing"
)

// hijackTruncate answers with headers plus a partial body, then drops the
// connection. A normal handler cannot do this: returning cleanly ends the
// chunked body and the client never sees a truncated read.
func hijackTruncate(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	hj, ok := w.(http.Hijacker)
	if !ok {
		t.Error("ResponseWriter does not support hijacking")
		return
	}
	c, buf, err := hj.Hijack()
	if err != nil {
		t.Errorf("hijack: %v", err)
		return
	}
	defer c.Close()
	_, _ = buf.WriteString("HTTP/1.1 200 OK\r\n" +
		"Content-Type: application/json\r\n" +
		"Content-Length: 200\r\n" +
		"Connection: close\r\n\r\n" +
		`{"id":"chatcmpl-1","choices":[{"index":0,`)
	_ = buf.Flush()
	// Declared 200 bytes, sent 37, then the deferred Close hangs up.
}

func TestTruncatedUpstreamBodyYields502NotEmpty200(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		hijackTruncate(t, w)
	})

	code, _, body := h.do("POST", "/v1/chat/completions", "tok-infer",
		`{"model":"up/gpt-6-luna","stream":false,"messages":[{"role":"user","content":"hi"}]}`)

	if code != http.StatusBadGateway {
		t.Fatalf("truncated upstream body answered %d with %q — want an explicit 502, never a silent empty 200", code, body)
	}
	if !strings.Contains(body, "upstream_error") {
		t.Fatalf("502 body lacks the upstream_error envelope: %s", body)
	}
	if !strings.Contains(body, "upstream request failed") {
		t.Fatalf("502 body does not explain the failure: %s", body)
	}
}
