package proxy

// Regression test for the empty-200 defect on commit()'s non-streaming path.
//
// When io.ReadAll failed on the upstream body, commit() returned the error
// WITHOUT ever calling w.WriteHeader — but it returned a Result whose Status
// field was still the upstream's status (set in attempt()), not 0. The
// server's error branch guarded on `res.Status == 0`, so a 200 passed the
// guard, net/http emitted its default 200, and the client received an empty
// 200 OK — the exact outcome that guard claims to prevent.
//
// The fix adds Result.Committed, set immediately before every WriteHeader in
// commit. The server gate is now `!res.Committed`. These tests pin the
// property the server gate depends on: a failed body read must leave
// Committed false, while Status is preserved for the ledger.

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// truncUpstream delivers response headers plus a partial body, then drops the
// connection, so the client's io.ReadAll fails mid-body while a status line has
// already been received.
func truncUpstream(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				// Drain just enough of the request for the client to finish
				// sending, then stop waiting — never block on EOF.
				_ = c.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
				_, _ = io.Copy(io.Discard, io.LimitReader(c, 1<<16))
				_, _ = c.Write([]byte(
					"HTTP/1.1 200 OK\r\n" +
						"Content-Type: application/json\r\n" +
						"Content-Length: 200\r\n" +
						"\r\n" +
						`{"id":"chatcmpl-1","choices":[{"index":0,`))
				// Declare 200 bytes, send 37, then hang up: the reader
				// gets an unexpected EOF with headers already delivered.
			}(c)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return "http://" + ln.Addr().String()
}

func TestNonStreamBodyReadFailureLeavesCommittedFalse(t *testing.T) {
	base := truncUpstream(t)

	client := &http.Client{Timeout: 30 * time.Second}
	d := NewDispatcher(client, provider.NewRegistry(client))
	rt := testRoute(base, provider.SurfaceOpenAI)

	body := []byte(`{"model":"m","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	req, _ := reqWithBody(string(body))

	// Reach the upstream the way commit() does, so the response object is real.
	resp, res, start, err := d.attempt(context.Background(), req, rt, body, nil)
	if err != nil {
		t.Fatalf("attempt: %v", err)
	}

	rec := httptest.NewRecorder()
	res2, cerr := d.commit(context.Background(), rec, resp, rt, res, start)

	if cerr == nil {
		t.Fatal("expected commit to return the body read error")
	}
	t.Logf("commit returned err=%v", cerr)

	// Nothing was written to the client (no status, no body) — commit failed
	// before its WriteHeader, exactly as before the fix.
	if rec.Body.Len() != 0 {
		t.Fatalf("expected an empty client body, got %d bytes: %q", rec.Body.Len(), rec.Body.String())
	}
	if rec.Flushed {
		t.Fatal("nothing should have been flushed")
	}

	// THE REGRESSION GUARD: the server gates its 502 on this flag, so a body
	// read failure must leave it false. Under the defect there was no flag at
	// all and the server fell through to net/http's empty 200.
	if res2.Committed {
		t.Fatal("Committed must be false when commit never wrote a status — otherwise the server's 502 gate stays silent and the client gets an empty 200")
	}
	// Status still carries the upstream's 200 so the ledger records what the
	// upstream actually answered; only Committed distinguishes "has a status"
	// from "has been sent".
	if res2.Status != http.StatusOK {
		t.Fatalf("expected Result.Status to keep the upstream 200 for the ledger, got %d", res2.Status)
	}
	if res2.Err == "" {
		t.Fatal("expected Result.Err to carry the read failure for the ledger")
	}
}

// TestCommitSetsCommittedOnSuccess pins the other side of the contract: every
// path that DOES write a status must report it, or the server would emit a
// duplicate 502 after a perfectly good response.
func TestCommitSetsCommittedOnSuccess(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","choices":[{"message":{"content":"OK"}}]}`))
	}))
	defer up.Close()

	client := up.Client()
	d := NewDispatcher(client, provider.NewRegistry(client))
	rt := testRoute(up.URL, provider.SurfaceOpenAI)

	body := []byte(`{"model":"m","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	req, _ := reqWithBody(string(body))

	resp, res, start, err := d.attempt(context.Background(), req, rt, body, nil)
	if err != nil {
		t.Fatalf("attempt: %v", err)
	}
	rec := httptest.NewRecorder()
	res2, err := d.commit(context.Background(), rec, resp, rt, res, start)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("client status = %d, want 200", rec.Code)
	}
	if !res2.Committed {
		t.Fatal("Committed must be true after commit wrote the upstream status")
	}
}

// TestCommitSetsCommittedOnEmptyBody pins the third WriteHeader: the 502 an
// empty upstream body produces already answered the client, so Committed must
// be true to stop the server from writing a second 502.
func TestCommitSetsCommittedOnEmptyBody(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// No body at all.
	}))
	defer up.Close()

	client := up.Client()
	d := NewDispatcher(client, provider.NewRegistry(client))
	rt := testRoute(up.URL, provider.SurfaceOpenAI)

	body := []byte(`{"model":"m","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	req, _ := reqWithBody(string(body))

	resp, res, start, err := d.attempt(context.Background(), req, rt, body, nil)
	if err != nil {
		t.Fatalf("attempt: %v", err)
	}
	rec := httptest.NewRecorder()
	res2, err := d.commit(context.Background(), rec, resp, rt, res, start)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("client status = %d, want the 502 commit already wrote", rec.Code)
	}
	if !res2.Committed {
		t.Fatal("Committed must be true — commit already answered the client with the 502")
	}
}
