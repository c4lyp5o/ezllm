package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A thinking model that requires the reasoning of every earlier assistant turn
// cannot serve a conversation another model started. It answers a 400 — but that
// 400 describes the HISTORY, not the body we sent, so committing it kills a
// client run that a hop of the same family could have served.
//
// Measured in the ledger (2026-10-24..25): 4 such 400s on cross-model failover,
// each one ending the run with "Missing required parameter:
// 'messages[2].reasoning_content'".
//
// The hop is NOT parked: it is perfectly healthy for a conversation it started
// itself, so parking it would take a good endpoint out of rotation for an
// unrelated reason.
func TestReasoningEchoRefusalFailsOverWithoutParking(t *testing.T) {
	refused := upstub(t, http.StatusBadRequest,
		`{"error":{"message":"The reasoning_content in the thinking mode must be passed back to the API.","type":"invalid_request_error"}}`)
	defer refused.Close()
	good := upstub(t, http.StatusOK, `{"ok":true,"model":"night"}`)
	defer good.Close()

	d := newDispatcher()
	req, body := reqWithBody(`{"model":"day","messages":[{"role":"user","content":"hi"}]}`)
	rec := httptest.NewRecorder()

	routes := []Route{textRoute(refused.URL, "day"), textRoute(good.URL, "night")}
	_, res, err := d.ForwardCandidates(context.Background(), rec, req, routes, body)
	if err != nil {
		t.Fatalf("ForwardCandidates: %v", err)
	}
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "night") {
		t.Fatalf("client saw %d %s, want hop 2's 200 — a history refusal must fail over", rec.Code, rec.Body.String())
	}
	if len(res.Attempts) != 1 || !strings.Contains(res.Attempts[0].Note, "[reasoning_echo_required]") {
		t.Errorf("attempt trail = %+v, want the refused hop carrying its own cause", res.Attempts)
	}
	if cos := d.Cooldowns(); len(cos) != 0 {
		t.Errorf("Cooldowns() = %+v, want none — the hop is healthy for conversations it started", cos)
	}
}
