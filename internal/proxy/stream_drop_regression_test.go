package proxy

// Regression test for the silent-EOF defect on mid-stream upstream failure.
//
// When the upstream drops the connection after the 200 status and some bytes
// have been committed, the proxy cannot change the status. Before this fix it
// simply returned, so the client saw an abrupt EOF with no error event — an
// indistinguishable outcome from a stream that ended early for any other
// reason. streamCopy now writes a terminal SSE error frame in the client's
// surface dialect before returning.

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// dropMidStreamUpstream sends a valid chunked SSE response with one event,
// then closes the connection without the terminating zero-length chunk. The
// client reads the event, then gets an unexpected EOF from the body reader.
func dropMidStreamUpstream(t *testing.T) string {
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
				_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
				_, _ = io.Copy(io.Discard, io.LimitReader(c, 1<<16))
				_, _ = c.Write([]byte(
					"HTTP/1.1 200 OK\r\n" +
						"Content-Type: text/event-stream\r\n" +
						"Transfer-Encoding: chunked\r\n" +
						"\r\n"))
				event := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n"
				_, _ = c.Write([]byte(strings.ToUpper(strings.TrimSpace(
					// hex chunk length
					hex(len(event)))) + "\r\n"))
				_, _ = c.Write([]byte(event + "\r\n"))
				// Drop without "0\r\n\r\n": the body is truncated mid-stream.
			}(c)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return "http://" + ln.Addr().String()
}

func hex(n int) string {
	const digits = "0123456789abcdef"
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{digits[n&15]}, b...)
		n >>= 4
	}
	return string(b)
}

func TestMidStreamDropWritesTerminalErrorFrame(t *testing.T) {
	base := dropMidStreamUpstream(t)
	client := &http.Client{Timeout: 5 * time.Second}
	d := NewDispatcher(client, provider.NewRegistry(client))
	rt := testRoute(base, provider.SurfaceChat)

	body := []byte(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	req, _ := reqWithBody(string(body))
	resp, res, start, err := d.attempt(context.Background(), req, rt, body, nil)
	if err != nil {
		t.Fatalf("attempt: %v", err)
	}
	rec := httptest.NewRecorder()
	res2, _ := d.commit(context.Background(), rec, resp, rt, res, start)

	if res2.Err == "" {
		t.Fatal("truncated stream must record an error in the ledger")
	}
	got := rec.Body.String()
	if !strings.Contains(got, `"error"`) || !strings.Contains(got, "upstream connection lost") {
		t.Fatalf("client body missing terminal SSE error frame: %q", got)
	}
	// The partial event must still have been forwarded.
	if !strings.Contains(got, `"content":"hi"`) {
		t.Fatalf("partial event not forwarded: %q", got)
	}
}
