package server

// A scoped sync scopes the SWEEP, never the catalog. Handing the filtered
// list to UpsertModels deleted every unnamed model (prod: command-code
// 87→3), so this pins the full refresh with a scoped probe request.

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestScopedSyncDoesNotPruneCatalog(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"data":[{"id":"m1"},{"id":"m2"},{"id":"m3"}]}`))
			return
		}
		// Probe traffic: answer plausibly; the verdict it records is irrelevant
		// here — the catalog is the subject.
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	})
	acct := seededAccountID(t, h)

	code, _, body := h.do("POST", fmt.Sprintf("/admin/accounts/%d/sync", acct), adminTok(), `{"models":["m1"]}`)
	if code != 200 {
		t.Fatalf("scoped sync: %d (%s), want 200", code, body)
	}

	got := map[string]bool{}
	for _, m := range listModels(t, h, acct) {
		got[m.ID] = true
	}
	for _, want := range []string{"m1", "m2", "m3"} {
		if !got[want] {
			t.Fatalf("scoped sync pruned the catalog: %q missing (have %v)", want, got)
		}
	}
}
