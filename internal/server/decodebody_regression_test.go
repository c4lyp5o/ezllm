package server

// C3 regression: three admin handlers (handleSetModelPin, handleSetPrice,
// handleDeletePrice) used a bare json.NewDecoder(r.Body).Decode(&body) while
// every other admin handler goes through decodeBody. That lost BOTH protections:
// the 1 MiB body cap (net/http imposes no default limit) and
// DisallowUnknownFields — so a typo'd field in a price payload was silently
// ignored instead of rejected, and one admin-authenticated caller could stream
// an unbounded body into memory.
//
// These tests pin both halves of the contract: an unknown field is a 400, and
// an over-cap body is a 400 rather than an OOM.

import (
	"net/http"
	"strings"
	"testing"
)

// A typo'd field name must be refused, not ignored. "Silently ignored" is how
// a setting that looks saved never actually saves.
func TestModelPinRejectsUnknownField(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	code, _, body := h.do("PUT", "/admin/accounts/1/model-pin", "tok-admin",
		`{"model":"gpt-6-luna","pinn":"openai"}`) // typo: pinn
	if code != 400 {
		t.Fatalf("unknown field answered %d, want 400: %s", code, body)
	}
	if !strings.Contains(body, "invalid_request_error") {
		t.Fatalf("body lacks the decodeBody envelope: %s", body)
	}
}

// The well-formed request still works: decodeBody must not reject valid input.
func TestModelPinAcceptsKnownFields(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	code, _, body := h.do("PUT", "/admin/accounts/1/model-pin", "tok-admin",
		`{"model":"gpt-6-luna","pin":"","force":false}`)
	if code != 200 {
		t.Fatalf("valid pin payload answered %d, want 200: %s", code, body)
	}
}

// The price handlers share the decoder, so the same typo must fail there too.
func TestSetPriceRejectsUnknownField(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	code, _, body := h.do("PUT", "/admin/accounts/1/prices", "tok-admin",
		`{"model":"gpt-6-luna","effective_from":"2026-10-11","currency":"USD",`+
			`"price_in":1.0,"price_out":2.0,"price_cache_read":0.5,"price_cache_write":1.5,"pric":9}`)
	if code != 400 {
		t.Fatalf("unknown field answered %d, want 400: %s", code, body)
	}
}

// A valid price payload must still land: decodeBody caps at 1 MiB, which no
// legitimate price row approaches.
func TestSetPriceAcceptsValidPayload(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	code, _, body := h.do("PUT", "/admin/accounts/1/prices", "tok-admin",
		`{"model":"gpt-6-luna","effective_from":"2026-10-11","currency":"USD",`+
			`"price_in":1.0,"price_out":2.0,"price_cache_read":0.5,"price_cache_write":1.5}`)
	if code != 201 && code != 200 {
		t.Fatalf("valid price payload answered %d, want 2xx: %s", code, body)
	}
}

// DELETE /prices takes the same body shape through the same decoder.
func TestDeletePriceRejectsUnknownField(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	code, _, body := h.do("DELETE", "/admin/accounts/1/prices", "tok-admin",
		`{"model":"gpt-6-luna","effective_from":"2026-10-11","modle":"gpt-6-luna"}`)
	if code != 400 {
		t.Fatalf("unknown field answered %d, want 400: %s", code, body)
	}
}

// An over-cap body is refused by the LimitReader instead of being buffered
// whole. The decoder surfaces it as a read error, which decodeBody maps to 400.
func TestModelPinRejectsOverCapBody(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	// 2 MiB of padding around a syntactically valid prefix — the decoder hits
	// the 1 MiB cap before EOF and reports "unexpected EOF".
	pad := strings.Repeat("A", 2<<20)
	code, _, body := h.do("PUT", "/admin/accounts/1/model-pin", "tok-admin",
		`{"model":"`+pad+`","pin":"openai"}`)
	if code != 400 {
		t.Fatalf("over-cap body answered %d, want 400: %s", code, body)
	}
}
