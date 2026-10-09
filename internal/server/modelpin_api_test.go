package server

// Model-pin admin API. Asserts the behaviours a wrong implementation would
// pass silently: an unconfirmed surface is refused (409) unless forced, an
// unknown model is 404 (never created), and bad bodies are 400.

import (
	"fmt"
	"net/http"
	"testing"
)

func seededAccountID(t *testing.T, h *harness) int64 {
	t.Helper()
	var id int64
	if err := h.db.Reader().QueryRow(`SELECT id FROM accounts LIMIT 1`).Scan(&id); err != nil {
		t.Fatalf("seeded account: %v", err)
	}
	return id
}

func pinPath(acct int64) string { return fmt.Sprintf("/admin/accounts/%d/model-pin", acct) }

func TestModelPinAPIValidation(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	acct := seededAccountID(t, h)
	cases := []struct {
		name, body string
		want       int
	}{
		{"missing model", `{"pin":"openai"}`, http.StatusBadRequest},
		{"absent pin is not a write", `{"model":"m"}`, http.StatusBadRequest},
		{"unknown surface", `{"model":"m","pin":"gemini"}`, http.StatusBadRequest},
		{"malformed json", `{"model":`, http.StatusBadRequest},
	}
	for _, c := range cases {
		code, _, resp := h.do("PUT", pinPath(acct), adminTok(), c.body)
		if code != c.want {
			t.Errorf("%s: got %d want %d (%s)", c.name, code, c.want, resp)
		}
	}
}

func TestModelPinUnknownModelIs404(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	acct := seededAccountID(t, h)
	code, _, resp := h.do("PUT", pinPath(acct), adminTok(), `{"model":"ghost","pin":"openai"}`)
	if code != http.StatusNotFound {
		t.Fatalf("unknown model: %d (%s), want 404", code, resp)
	}
}

func TestModelPinRefusesUnconfirmedUnlessForced(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	acct := seededAccountID(t, h)
	// Probe confirmed openai only; responses is unconfirmed.
	if _, err := h.db.Writer().Exec(
		`INSERT INTO models (account_id, model_id, proto_openai, proto_anthropic, proto_responses)
		 VALUES (?, 'probed', 1, 0, 0)`, acct); err != nil {
		t.Fatal(err)
	}

	code, _, resp := h.do("PUT", pinPath(acct), adminTok(), `{"model":"probed","pin":"responses"}`)
	if code != http.StatusConflict {
		t.Fatalf("unconfirmed pin: %d (%s), want 409", code, resp)
	}
	code, _, resp = h.do("PUT", pinPath(acct), adminTok(), `{"model":"probed","pin":"responses","force":true}`)
	if code != http.StatusOK {
		t.Fatalf("forced pin: %d (%s), want 200", code, resp)
	}
	code, _, resp = h.do("PUT", pinPath(acct), adminTok(), `{"model":"probed","pin":"openai"}`)
	if code != http.StatusOK {
		t.Fatalf("confirmed pin: %d (%s), want 200", code, resp)
	}
	code, _, resp = h.do("PUT", pinPath(acct), adminTok(), `{"model":"probed","pin":""}`)
	if code != http.StatusOK {
		t.Fatalf("clear: %d (%s), want 200", code, resp)
	}
}
