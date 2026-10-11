package proxy

// F4 regression: the SSE branch of commit() set res.Err only for a COPY
// failure. The non-stream branch had an explicit fix for this (an upstream
// error status records the provider's own sentence), but streams had no
// equivalent — so a failover-committed SSE error response landed in the ledger
// with, e.g., status 429 and a BLANK err column: the one field the proxy's own
// comment calls "the single most useful field for diagnosing an incident".
//
// Reachable through ForwardCandidates' fallback path: every hop fails with a
// failover status, and the last stored response — an SSE one — is committed.
// This test drives that exact path.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// sseErrUpstream is a 429 delivered as an SSE body with the reason inside the
// first data payload — the shape that produced a blank err column.
func sseErrUpstream() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(
			"data: {\"error\":{\"type\":\"rate_limit_error\",\"message\":\"Maximum 5 requests within 1 minutes\"}}\n\n" +
				"data: [DONE]\n\n"))
	}))
}

func TestSSEErrorStreamRecordsLedgerErr(t *testing.T) {
	up := sseErrUpstream()
	defer up.Close()

	client := up.Client()
	d := NewDispatcher(client, provider.NewRegistry(client))
	rt := testRoute(up.URL, provider.SurfaceOpenAI)

	// A stream request: this selects commit's SSE branch.
	res := Result{Status: http.StatusTooManyRequests, Stream: true}

	req, _ := reqWithBody(`{"model":"m","stream":true,"messages":[]}`)
	resp, _, _, err := d.attempt(context.Background(), req, rt, []byte(`{"model":"m","stream":true,"messages":[]}`))
	if err != nil {
		t.Fatalf("attempt: %v", err)
	}

	rec := httptest.NewRecorder()
	res2, cerr := d.commit(context.Background(), rec, resp, rt, res, time.Now())
	if cerr != nil {
		t.Fatalf("commit: %v", cerr)
	}

	if res2.Err == "" {
		t.Fatal("F4 regression: SSE error stream left the ledger err column blank")
	}
	if want := "Maximum 5 requests within 1 minutes"; !strings.Contains(res2.Err, want) {
		t.Fatalf("ledger err = %q, want it to quote the provider's reason %q", res2.Err, want)
	}
	if !strings.Contains(res2.Err, "upstream 429") {
		t.Fatalf("ledger err = %q, want the upstream 429 status prefix", res2.Err)
	}
	// The stream still reached the client untouched.
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("client status = %d, want the upstream 429 forwarded", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Maximum 5 requests") {
		t.Fatalf("SSE payload not forwarded to the client: %s", rec.Body.String())
	}
	if !res2.Committed {
		t.Fatal("Committed must be true: streamCopy already wrote the status")
	}
}

// A 200 stream carrying an error object inside the payload must also land in
// the err column — a provider's own refusal is not a success, and the status
// line does not say so.
func TestSSE200WithErrorEnvelopeRecordsLedgerErr(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"data: {\"error\":{\"type\":\"plan_gated\",\"message\":\"this model is not included in your free usage\"}}\n\n"))
	}))
	defer up.Close()

	client := up.Client()
	d := NewDispatcher(client, provider.NewRegistry(client))
	rt := testRoute(up.URL, provider.SurfaceOpenAI)

	res := Result{Status: http.StatusOK, Stream: true}
	req, _ := reqWithBody(`{"model":"m","stream":true,"messages":[]}`)
	resp, _, _, err := d.attempt(context.Background(), req, rt, []byte(`{"model":"m","stream":true,"messages":[]}`))
	if err != nil {
		t.Fatalf("attempt: %v", err)
	}
	rec := httptest.NewRecorder()
	res2, cerr := d.commit(context.Background(), rec, resp, rt, res, time.Now())
	if cerr != nil {
		t.Fatalf("commit: %v", cerr)
	}
	if res2.Err == "" {
		t.Fatal("a 200 stream carrying an error envelope left the ledger err blank")
	}
	if !strings.Contains(res2.Err, "not included in your free usage") {
		t.Fatalf("ledger err = %q, want the provider's refusal reason", res2.Err)
	}
}

// A clean stream must keep a blank err: the fix may not brand good responses.
func TestSSECleanStreamKeepsBlankErr(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\n" +
				"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n" +
				"data: [DONE]\n\n"))
	}))
	defer up.Close()

	client := up.Client()
	d := NewDispatcher(client, provider.NewRegistry(client))
	rt := testRoute(up.URL, provider.SurfaceOpenAI)

	res := Result{Status: http.StatusOK, Stream: true}
	req, _ := reqWithBody(`{"model":"m","stream":true,"messages":[]}`)
	resp, _, _, err := d.attempt(context.Background(), req, rt, []byte(`{"model":"m","stream":true,"messages":[]}`))
	if err != nil {
		t.Fatalf("attempt: %v", err)
	}
	rec := httptest.NewRecorder()
	res2, cerr := d.commit(context.Background(), rec, resp, rt, res, time.Now())
	if cerr != nil {
		t.Fatalf("commit: %v", cerr)
	}
	if res2.Err != "" {
		t.Fatalf("clean stream recorded an error: %q", res2.Err)
	}
	if !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("clean stream not forwarded: %s", rec.Body.String())
	}
}
