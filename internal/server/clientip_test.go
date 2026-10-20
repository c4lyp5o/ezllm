package server

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func mustProxies(t *testing.T, s string) []netip.Prefix {
	t.Helper()
	p, err := ParseTrustedProxies(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseTrustedProxies(t *testing.T) {
	p := mustProxies(t, " 172.18.0.0/16, 10.0.0.5 ,, ::1 ")
	if len(p) != 3 || p[0].String() != "172.18.0.0/16" || p[1].String() != "10.0.0.5/32" || p[2].String() != "::1/128" {
		t.Fatalf("parsed %v", p)
	}
	if p, err := ParseTrustedProxies(""); err != nil || len(p) != 0 {
		t.Fatalf("empty = %v, %v", p, err)
	}
	for _, bad := range []string{"172.18.0.0/99", "not-an-ip", "10.0.0.1, nope"} {
		if _, err := ParseTrustedProxies(bad); err == nil {
			t.Fatalf("%q must be refused", bad)
		}
	}
}

func TestSourceIP(t *testing.T) {
	cases := []struct {
		name, trusted, peer, xff, realIP, want string
	}{
		{"no trusted proxies ignores headers", "", "198.51.100.7:4000", "203.0.113.5", "203.0.113.9", "198.51.100.7"},
		{"untrusted peer ignores headers", "10.0.0.0/8", "198.51.100.7:4000", "203.0.113.5", "", "198.51.100.7"},
		{"trusted peer, one hop", "10.0.0.0/8", "10.0.0.2:4000", "203.0.113.5", "", "203.0.113.5"},
		{"client-sent spoof on the left is skipped", "10.0.0.0/8", "10.0.0.2:4000", "1.2.3.4, 203.0.113.5", "", "203.0.113.5"},
		{"chained trusted proxies are walked past", "10.0.0.0/8", "10.0.0.2:4000", "203.0.113.5, 10.0.0.9", "", "203.0.113.5"},
		{"malformed rightmost hop falls back to peer", "10.0.0.0/8", "10.0.0.2:4000", "203.0.113.5, junk", "", "10.0.0.2"},
		{"x-real-ip from a trusted peer without xff", "10.0.0.0/8", "10.0.0.2:4000", "", "203.0.113.9", "203.0.113.9"},
		{"ipv4-mapped peer still matches", "10.0.0.0/8", "[::ffff:10.0.0.2]:4000", "203.0.113.5", "", "203.0.113.5"},
	}
	for _, c := range cases {
		s := &Server{trustedProxies: mustProxies(t, c.trusted)}
		r := httptest.NewRequest("POST", "/admin/login", nil)
		r.RemoteAddr = c.peer
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if c.realIP != "" {
			r.Header.Set("X-Real-Ip", c.realIP)
		}
		if got := s.sourceIP(r); got != c.want {
			t.Errorf("%s: sourceIP = %q, want %q", c.name, got, c.want)
		}
	}
}

func loginFrom(h *harness, xff, pw string) int {
	req := httptest.NewRequest("POST", "/admin/login", strings.NewReader(`{"password":"`+pw+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", xff)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec.Code
}

// M3: behind a reverse proxy every client arrived from the proxy's address,
// so five wrong guesses from anyone locked sign-in for everyone. With the
// proxy trusted, the limiter keys on the real client.
func TestLoginLimiterKeysOnClientBehindTrustedProxy(t *testing.T) {
	// httptest requests come from 192.0.2.1: that is "the proxy" here.
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {}, func(o *Options) {
		o.TrustedProxies = mustProxies(t, "192.0.2.0/24")
	})
	setupDashboard(t, h, "proxy-limit-password")
	for _, ip := range []string{"192.0.2.1", "203.0.113.5", "203.0.113.6"} {
		clearLoginAttempts(ip)
		defer clearLoginAttempts(ip)
	}
	for i := 0; i < 5; i++ {
		if code := loginFrom(h, "203.0.113.5", "wrong"); code != http.StatusUnauthorized {
			t.Fatalf("attacker attempt %d = %d, want 401", i+1, code)
		}
	}
	if code := loginFrom(h, "203.0.113.5", "wrong"); code != http.StatusTooManyRequests {
		t.Fatalf("attacker sixth attempt = %d, want 429", code)
	}
	if code := loginFrom(h, "203.0.113.6", "proxy-limit-password"); code != http.StatusOK {
		t.Fatalf("a different client behind the same proxy = %d, want 200 (not locked out)", code)
	}
}

// Without trusted proxies, invented headers must not dodge the limiter.
func TestLoginLimiterIgnoresSpoofedForwardedFor(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	setupDashboard(t, h, "spoof-limit-password")
	clearLoginAttempts("192.0.2.1")
	defer clearLoginAttempts("192.0.2.1")
	for i := 0; i < 5; i++ {
		loginFrom(h, "203.0.113."+string(rune('1'+i)), "wrong")
	}
	if code := loginFrom(h, "203.0.113.99", "wrong"); code != http.StatusTooManyRequests {
		t.Fatalf("rotating X-Forwarded-For without a trusted proxy = %d, want 429", code)
	}
}
