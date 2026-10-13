package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/proxy"
)

// The hold-until-failure release rule (operator decision 2026-10-13): only a
// failure that is the HOP's fault releases the block. A 4xx is the caller's
// payload or model id and must keep the hold, or a single bad request would
// rotate away from a perfectly healthy hop.

func TestHopAttributableFailure(t *testing.T) {
	cases := []struct {
		name string
		res  proxy.Result
		ferr error
		want bool
	}{
		{"delivered 200", proxy.Result{Status: 200}, nil, false},
		{"redirect", proxy.Result{Status: 302}, nil, false},
		{"bad request is the caller's fault", proxy.Result{Status: 400}, nil, false},
		{"not found is the caller's model id", proxy.Result{Status: 404}, nil, false},
		{"rate limited is the hop's budget", proxy.Result{Status: 429}, nil, true},
		{"server error is the hop", proxy.Result{Status: 500}, nil, true},
		{"bad gateway is the hop", proxy.Result{Status: 502}, nil, true},
		{"transport error never reached a body", proxy.Result{Status: 0}, errors.New("dial: refused"), true},
		{"stream completed", proxy.Result{Status: 200, StreamOutcome: proxy.StreamOutcomeCompleted}, nil, false},
		{"stream failed late", proxy.Result{Status: 200, StreamOutcome: proxy.StreamOutcomeFailed}, nil, true},
		{"stream truncated", proxy.Result{Status: 200, StreamOutcome: proxy.StreamOutcomeTruncated}, nil, true},
	}
	for _, c := range cases {
		if got := hopAttributableFailure(c.res, c.ferr); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// clientIP precedence (X-Real-IP, then the FIRST X-Forwarded-For entry, then
// the socket peer) and the ledger's "empty when unavailable" contract.
func TestClientIP(t *testing.T) {
	newReq := func(remote, realIP, fwd string) *http.Request {
		r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		r.RemoteAddr = remote
		if realIP != "" {
			r.Header.Set("X-Real-Ip", realIP)
		}
		if fwd != "" {
			r.Header.Set("X-Forwarded-For", fwd)
		}
		return r
	}
	if got := clientIP(newReq("10.0.0.7:5555", "203.0.113.9", "198.51.100.1, 10.0.0.1")); got != "203.0.113.9" {
		t.Errorf("X-Real-IP must win: got %q", got)
	}
	if got := clientIP(newReq("10.0.0.7:5555", "", "198.51.100.1, 10.0.0.1")); got != "198.51.100.1" {
		t.Errorf("first X-Forwarded-For entry: got %q", got)
	}
	if got := clientIP(newReq("10.0.0.7:5555", "", "")); got != "10.0.0.7" {
		t.Errorf("socket peer: got %q", got)
	}
	if got := clientIP(newReq("10.0.0.7:5555", "", "  198.51.100.1  , 10.0.0.1  ")); got != "198.51.100.1" {
		t.Errorf("whitespace around the header must be trimmed: got %q", got)
	}
}

// The capture must reach the LEDGER: a request carrying a forwarded address
// lands in calls.ip, and a request without any address info lands empty —
// the column must never be filled with a guess.
func TestRequestIPStoredInLedger(t *testing.T) {
	fake := &fakeUpstream{}
	h := newHarness(t, fake.handler())

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"up/gpt-6-luna","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`))
	req.Header.Set("Authorization", "Bearer tok-infer")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", "198.51.100.7, 10.0.0.1")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if err := h.db.Flush(); err != nil {
		t.Fatal(err)
	}
	rows, err := h.db.RecentCalls(context.Background(), 2)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	if rows[0].IP != "198.51.100.7" {
		t.Errorf("ledger ip = %q, want the first forwarded address", rows[0].IP)
	}

	req2 := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"up/gpt-6-luna","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`))
	req2.Header.Set("Authorization", "Bearer tok-infer")
	req2.Header.Set("Content-Type", "application/json")
	req2.RemoteAddr = "10.0.0.7:4242"
	rec2 := httptest.NewRecorder()
	h.handler.ServeHTTP(rec2, req2)
	if err := h.db.Flush(); err != nil {
		t.Fatal(err)
	}
	rows2, _ := h.db.RecentCalls(context.Background(), 1)
	if rows2[0].IP != "10.0.0.7" {
		t.Errorf("ledger ip = %q, want the socket peer when no header exists", rows2[0].IP)
	}

	// X-Real-IP outranks X-Forwarded-For end to end (nginx's header wins over
	// an appended chain) — the precedence must survive into the LEDGER, not
	// just the helper.
	req3 := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"up/gpt-6-luna","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`))
	req3.Header.Set("Authorization", "Bearer tok-infer")
	req3.Header.Set("Content-Type", "application/json")
	req3.Header.Set("X-Real-Ip", "203.0.113.44")
	req3.Header.Set("X-Forwarded-For", "198.51.100.9")
	rec3 := httptest.NewRecorder()
	h.handler.ServeHTTP(rec3, req3)
	if err := h.db.Flush(); err != nil {
		t.Fatal(err)
	}
	rows3, _ := h.db.RecentCalls(context.Background(), 1)
	if rows3[0].IP != "203.0.113.44" {
		t.Errorf("ledger ip = %q, want X-Real-IP to outrank X-Forwarded-For", rows3[0].IP)
	}
}
