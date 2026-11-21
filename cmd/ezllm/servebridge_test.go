package main

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"
)

// serveBridge serves the handler on the address and shutdownBridgeHTTP stops it.
func TestServeBridgeServesAndStops(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	srv := serveBridge(slog.New(slog.NewTextHandler(io.Discard, nil)), "test", addr,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "up") }))
	var body []byte
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if resp, err := http.Get("http://" + addr); err == nil {
			body, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			break
		}
	}
	if string(body) != "up" {
		t.Fatalf("body = %q, want the handler's answer", body)
	}
	if srv.ReadHeaderTimeout != 10*time.Second || srv.IdleTimeout != 2*time.Minute {
		t.Fatalf("timeouts = %s/%s", srv.ReadHeaderTimeout, srv.IdleTimeout)
	}
	shutdownBridgeHTTP(srv)
	if _, err := http.Get("http://" + addr); err == nil {
		t.Fatal("still serving after shutdown")
	}
}
