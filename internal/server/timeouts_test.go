package server

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// M4: with only ReadHeaderTimeout set, a client could send headers and then
// trickle (or never send) the body, pinning the handler — on /admin/login,
// without authenticating. The body read now has its own budget.
func TestTrickledBodyIsCutOff(t *testing.T) {
	old := adminBodyTimeout
	adminBodyTimeout = 200 * time.Millisecond
	defer func() { adminBodyTimeout = old }()

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	setupDashboard(t, h, "trickle-test-password")
	srv := httptest.NewServer(h.handler)
	defer srv.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	start := time.Now()
	// Promise 100 bytes, send 9, then go quiet.
	fmt.Fprint(conn, "POST /admin/login HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"passwor")
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no response to a stalled body (handler pinned?): %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("stalled body = %d, want 400", resp.StatusCode)
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("stalled body answered after %v; the budget is 200ms", el)
	}
}

// The body budget covers the body only: a response that streams for longer
// than the budget must arrive whole.
func TestStreamOutlivesTheBodyBudget(t *testing.T) {
	old := inferenceBodyTimeout
	inferenceBodyTimeout = 100 * time.Millisecond
	defer func() { inferenceBodyTimeout = old }()

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; i < 6; i++ {
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"part%d\"}}]}\n\n", i)
			fl.Flush()
			time.Sleep(100 * time.Millisecond) // the whole stream runs ~6x the budget
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	})
	srv := httptest.NewServer(h.handler)
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"up/gpt-6-luna","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer tok-infer")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("stream cut short: %v (got %q)", err, body)
	}
	if resp.StatusCode != 200 || !strings.Contains(string(body), "part5") || !strings.Contains(string(body), "[DONE]") {
		t.Fatalf("stream = %d, want every chunk:\n%s", resp.StatusCode, body)
	}
}
