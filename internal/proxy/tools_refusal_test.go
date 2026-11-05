package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The Copilot CLI bridge runs its own agent loop and refuses client tool
// schemas with a 400 coded chat_tools_unsupported. That 400 describes the HOP,
// so a combo must fail over to a hop that takes tools instead of ending the
// client run (measured 2026-11-05: client maxine, Copilot first in its combo,
// three working hops behind it). The refused hop is not parked: it still serves
// requests without tools.
func TestToolsRefusalFailsOverWithoutParking(t *testing.T) {
	refused := upstub(t, http.StatusBadRequest,
		`{"error":{"message":"copilot-bridge: this Copilot hop runs the CLI's own agent loop and cannot accept client tool schemas","type":"invalid_request_error","code":"chat_tools_unsupported"}}`)
	defer refused.Close()
	good := upstub(t, http.StatusOK, `{"ok":true,"model":"capable"}`)
	defer good.Close()

	d := newDispatcher()
	req, body := reqWithBody(`{"model":"day","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}]}`)
	rec := httptest.NewRecorder()

	routes := []Route{textRoute(refused.URL, "day"), textRoute(good.URL, "capable")}
	_, res, err := d.ForwardCandidates(context.Background(), rec, req, routes, body)
	if err != nil {
		t.Fatalf("ForwardCandidates: %v", err)
	}
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "capable") {
		t.Fatalf("client saw %d %s, want hop 2's 200 — a tools refusal must fail over", rec.Code, rec.Body.String())
	}
	if len(res.Attempts) != 1 || !strings.Contains(res.Attempts[0].Note, "[chat_tools_unsupported]") {
		t.Errorf("attempt trail = %+v, want the refused hop carrying its own cause", res.Attempts)
	}
	if cos := d.Cooldowns(); len(cos) != 0 {
		t.Errorf("Cooldowns() = %+v, want none — the hop is healthy for tool-less requests", cos)
	}
}
