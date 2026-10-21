package main

import (
	"testing"
)

// The in-process bridges dial the server's own listener. A wildcard bind
// (0.0.0.0, [::], ":port") is not an address to dial; loopback is.
func TestBridgeBaseIsLoopbackForWildcardBinds(t *testing.T) {
	for addr, want := range map[string]string{
		":20129":         "http://127.0.0.1:20129",
		"0.0.0.0:20129":  "http://127.0.0.1:20129",
		"[::]:20129":     "http://127.0.0.1:20129",
		"127.0.0.1:9000": "http://127.0.0.1:9000",
		"10.0.0.5:20129": "http://10.0.0.5:20129",
	} {
		if got := bridgeOrigin(addr); got != want {
			t.Errorf("bridgeOrigin(%q) = %q, want %q", addr, got, want)
		}
	}
}
