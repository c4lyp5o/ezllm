package server

// M1 server boundary: wire-first selection's typed refusal must reach the
// client as a 400 (the model's surface is probed impossible and nothing
// entitles it to another wire), never as a gateway 502 or a wasted upstream
// call. The untested-control case pins the other half: NULL capability keeps
// serving native traffic exactly as before.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestProvenImpossibleWireIs400NotAnUpstreamCall(t *testing.T) {
	var upstreamCalls int
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	})
	acct := seededAccountID(t, h)

	// Probe the model as UNABLE to serve the anthropic wire, with no
	// openai proof — nothing entitles this route to any wire but the one
	// that is disproved.
	if _, err := h.db.Writer().Exec(
		`INSERT INTO models (account_id, model_id, proto_anthropic) VALUES (?, 'wireless', 0)`,
		acct); err != nil {
		t.Fatal(err)
	}
	var ns string
	if err := h.db.Reader().QueryRow(`SELECT namespace FROM accounts WHERE id=?`, acct).Scan(&ns); err != nil {
		t.Fatal(err)
	}

	code, _, body := h.do("POST", "/v1/messages", "tok-infer",
		`{"model":"`+ns+`/wireless","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", code, body)
	}
	var e struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("unparseable body: %v (%s)", err, body)
	}
	if e.Error.Type != "wire_unsupported" {
		t.Errorf("error type = %q, want wire_unsupported (%s)", e.Error.Type, body)
	}
	if !strings.Contains(e.Error.Message, "wireless") {
		t.Errorf("message = %q, want the model named", e.Error.Message)
	}
	if upstreamCalls != 0 {
		t.Errorf("a proven-impossible wire still cost %d upstream call(s)", upstreamCalls)
	}
}

func TestUntestedWireStillServesNative(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	})
	acct := seededAccountID(t, h)
	// Same shape of row, but no verdict at all: NULL = never probed.
	if _, err := h.db.Writer().Exec(
		`INSERT INTO models (account_id, model_id) VALUES (?, 'untested-wire')`,
		acct); err != nil {
		t.Fatal(err)
	}
	var ns string
	if err := h.db.Reader().QueryRow(`SELECT namespace FROM accounts WHERE id=?`, acct).Scan(&ns); err != nil {
		t.Fatal(err)
	}

	code, _, body := h.do("POST", "/v1/messages", "tok-infer",
		`{"model":"`+ns+`/untested-wire","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if code != http.StatusOK {
		t.Fatalf("untested model on its native wire: %d (%s), want 200 — NULL must never be read as impossible", code, body)
	}
}
