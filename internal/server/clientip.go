package server

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// ParseTrustedProxies reads EZLLM_TRUSTED_PROXIES: a comma-separated list of
// IPs or CIDRs (e.g. "172.18.0.0/16, 10.0.0.5"). Empty means none, so
// forwarding headers are never believed. A malformed entry is an error: a
// typo silently trusting nothing (or everything) is worse than a failed boot.
func ParseTrustedProxies(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if strings.Contains(f, "/") {
			p, err := netip.ParsePrefix(f)
			if err != nil {
				return nil, fmt.Errorf("trusted proxy %q: %w", f, err)
			}
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(f)
		if err != nil {
			return nil, fmt.Errorf("trusted proxy %q: %w", f, err)
		}
		a = a.Unmap()
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

func (s *Server) isTrustedProxy(a netip.Addr) bool {
	a = a.Unmap()
	for _, p := range s.trustedProxies {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// sourceIP is the caller address the auth rate limiter keys on. Forwarding
// headers are believed ONLY when the socket peer is a configured trusted
// proxy; then X-Forwarded-For is walked right to left past trusted hops and
// the first untrusted address is the client (the leftmost entries are
// whatever the client sent and are never trusted). Without trusted proxies
// it is the socket peer, so a client cannot dodge the limiter by inventing
// headers — and behind a proxy, every client no longer shares the proxy's
// one bucket.
//
// This is a security decision, unlike clientIP (ledger attribution), which
// takes headers at face value.
func (s *Server) sourceIP(r *http.Request) string {
	peer := r.RemoteAddr
	if host, _, err := net.SplitHostPort(peer); err == nil {
		peer = host
	}
	pa, err := netip.ParseAddr(peer)
	if err != nil || len(s.trustedProxies) == 0 || !s.isTrustedProxy(pa) {
		return peer
	}
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			// A malformed hop: stop rather than skip, or a junk entry would
			// let the walk reach client-controlled addresses.
			break
		}
		if !s.isTrustedProxy(a) {
			return a.Unmap().String()
		}
	}
	if a, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get("X-Real-Ip"))); err == nil && len(hops) == 0 {
		return a.Unmap().String()
	}
	return peer
}
