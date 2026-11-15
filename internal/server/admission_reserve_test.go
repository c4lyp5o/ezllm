package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A declared Content-Length is reserved exactly, so small bodies are limited
// by the inflight limits rather than by max_body-sized reservations.
func TestAdmissionReservesDeclaredLengthNotTheWholeCap(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {}, func(o *Options) {
		o.MaxBodyMiB = 2
		o.RequestLimits = RequestLimits{GlobalInflight: 16, PerClientInflight: 16, BodyMemoryMiB: 4}
	})
	var leases []*requestLease
	defer func() {
		for _, l := range leases {
			l.release()
		}
	}()
	for i := 0; i < 10; i++ {
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"x"}`))
		r.Header.Set("Authorization", "Bearer tok-infer")
		l := h.server.admit(httptest.NewRecorder(), r)
		if l == nil {
			t.Fatalf("request %d with a %d-byte body refused under a 4 MiB budget", i, r.ContentLength)
		}
		leases = append(leases, l)
	}

	// Unknown length still reserves the whole cap: two fill the 4 MiB budget.
	for _, l := range leases {
		l.release()
	}
	leases = nil
	for i := 0; i < 3; i++ {
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
		r.ContentLength = -1
		r.Header.Set("Authorization", "Bearer tok-infer")
		l := h.server.admit(httptest.NewRecorder(), r)
		if (l != nil) != (i < 2) {
			t.Fatalf("unknown-length request %d admitted=%v, want %v", i, l != nil, i < 2)
		}
		if l != nil {
			leases = append(leases, l)
		}
	}
}
