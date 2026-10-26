package proxy

// F3 regression: the non-streaming commit path used to read the upstream body
// through a bare io.LimitReader(resp.Body, maxBodyBytes). Hitting the cap was
// NOT an error — the truncated JSON was written to the client with a 200 and a
// Content-Length that no longer matched the body. Large reasoning responses
// and big tool payloads can legitimately reach 32 MiB.
//
// The fix reads one byte MORE than the cap so an over-cap body is detectable,
// and refuses with 502 upstream_response_truncated instead of writing partial
// JSON. The cap is deliberately NOT raised: the body is buffered in memory.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// repeatReader serves its buffer forever, so a test can describe an upstream
// larger than maxBodyBytes without allocating 32 MiB.
type repeatReader struct {
	b   []byte
	off int
}

func (r *repeatReader) Read(p []byte) (int, error) {
	n := copy(p, r.b[r.off:])
	r.off = (r.off + n) % len(r.b)
	return n, nil
}

// overCapResponse is a 200 JSON upstream whose body runs past maxBodyBytes.
func overCapResponse() *http.Response {
	filler := []byte(`{"id":"x","padding":"` + strings.Repeat("A", 4096) + `"}`)
	return &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		Body:          io.NopCloser(io.LimitReader(&repeatReader{b: filler}, maxBodyBytes+4096)),
		ContentLength: -1,
	}
}

func TestOverCapNonStreamBodyYields502NotPartialJSON(t *testing.T) {
	client := &http.Client{Timeout: 30 * time.Second}
	d := NewDispatcher(client, provider.NewRegistry(client))
	rt := testRoute("http://upstream.invalid", provider.SurfaceChat)

	res := Result{Status: http.StatusOK, Stream: false}
	rec := httptest.NewRecorder()

	res2, err := d.commit(context.Background(), rec, overCapResponse(), rt, res, time.Now())
	if err != nil {
		t.Fatalf("commit must refuse the over-cap body without a transport error, got: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("over-cap body answered %d, want 502 (a truncated 200 is the defect)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "upstream_response_truncated") {
		t.Fatalf("502 body lacks the truncated envelope: %s", rec.Body.String())
	}
	if rec.Body.Len() > 4096 {
		t.Fatalf("client received %d bytes — partial payload was written", rec.Body.Len())
	}
	if !res2.Committed {
		t.Fatal("Committed must be true: the 502 was already written to the client")
	}
	if res2.Status != http.StatusBadGateway {
		t.Fatalf("ledger status = %d, want the 502 that was served", res2.Status)
	}
	if !strings.Contains(res2.Err, "exceeded") {
		t.Fatalf("ledger err must name the cause: %q", res2.Err)
	}
}

// A body exactly at the cap must still pass through untouched: the fix may not
// reject legitimate responses.
func TestBodyExactlyAtCapStillServed(t *testing.T) {
	client := &http.Client{Timeout: 30 * time.Second}
	d := NewDispatcher(client, provider.NewRegistry(client))
	rt := testRoute("http://upstream.invalid", provider.SurfaceChat)

	// A loud, parseable JSON payload of exactly maxBodyBytes bytes.
	pad := strings.Repeat("A", maxBodyBytes-len(`{"id":"x","pad":""}`))
	exact := []byte(`{"id":"x","pad":"` + pad + `"}`)
	if int64(len(exact)) != maxBodyBytes {
		t.Fatalf("fixture is %d bytes, want exactly %d", len(exact), maxBodyBytes)
	}
	resp := &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		Body:          io.NopCloser(strings.NewReader(string(exact))),
		ContentLength: int64(len(exact)),
	}

	res := Result{Status: http.StatusOK, Stream: false}
	rec := httptest.NewRecorder()
	if _, err := d.commit(context.Background(), rec, resp, rt, res, time.Now()); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("at-cap body answered %d, want 200", rec.Code)
	}
	if rec.Body.Len() != len(exact) {
		t.Fatalf("at-cap body wrote %d of %d bytes", rec.Body.Len(), len(exact))
	}
}
