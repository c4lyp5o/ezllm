package server

// The "test all models" action: probe the WHOLE catalog on the requested
// surfaces, skip what already carries a verdict, and stay observable/cancellable
// (a multi-minute background job must not be a blind wait).

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func sweepUpstream(models []string, perProbe time.Duration) http.HandlerFunc {
	list := `{"data":[`
	for i, m := range models {
		if i > 0 {
			list += ","
		}
		list += fmt.Sprintf(`{"id":%q}`, m)
	}
	list += `]}`
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(list))
			return
		}
		time.Sleep(perProbe)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}
}

// waitSweepDone polls the status route until the sweep finishes (or fails).
func waitSweepDone(t *testing.T, h *harness, acct int64, limit time.Duration) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		code, _, body := h.do("GET", fmt.Sprintf("/admin/accounts/%d/sync", acct), adminTok(), "")
		if code != 200 {
			t.Fatalf("status: %d (%s)", code, body)
		}
		if strings.Contains(body, `"running":false`) {
			return
		}
		time.Sleep(75 * time.Millisecond)
	}
	t.Fatal("sweep did not finish in time")
}

func verdictsFor(t *testing.T, h *harness, acct int64) (tested, untested int) {
	t.Helper()
	for _, m := range listModels(t, h, acct) {
		if m.Chat == nil && m.Anthropic == nil && m.Responses == nil {
			untested++
		} else {
			tested++
		}
	}
	return tested, untested
}

// Every model gets probed — the cap that made the old sweep a 6-of-N sample is
// lifted, and a second click does not re-spend on models that already have a
// verdict on every requested surface.
func TestProbeAllSweepsWholeCatalog(t *testing.T) {
	models := []string{"m1", "m2", "m3", "m4", "m5", "m6", "m7", "m8"}
	h := newHarness(t, sweepUpstream(models, 30*time.Millisecond))
	acct := seededAccountID(t, h)

	code, _, body := h.do("POST", fmt.Sprintf("/admin/accounts/%d/sync", acct), adminTok(),
		`{"probe_all":true}`)
	if code != 200 {
		t.Fatalf("probe_all sync: %d (%s)", code, body)
	}
	// The harness catalog seeds m... wait: assert the reported work first.
	if !strings.Contains(body, `"probing":8`) {
		t.Fatalf("probing count = %s, want 8 (the whole catalog, not the 6-model sample)", body)
	}
	waitSweepDone(t, h, acct, 20*time.Second)

	tested, untested := verdictsFor(t, h, acct)
	if tested != len(models) || untested != 0 {
		t.Fatalf("after probe_all: tested=%d untested=%d, want %d/0", tested, untested, len(models))
	}

	// Second click: everything carries verdicts, so there is nothing to spend.
	code, _, body = h.do("POST", fmt.Sprintf("/admin/accounts/%d/sync", acct), adminTok(), `{"probe_all":true}`)
	if code != 200 || !strings.Contains(body, `"probing":0`) {
		t.Fatalf("re-probe of a fully tested catalog: %d (%s), want 0 to probe", code, body)
	}

	// A forced re-test must still sweep everything (the knobs are independent).
	code, _, body = h.do("POST", fmt.Sprintf("/admin/accounts/%d/sync", acct), adminTok(),
		`{"probe_all":true,"only_untested":false}`)
	if code != 200 || !strings.Contains(body, `"probing":8`) {
		t.Fatalf("forced re-probe: %d (%s), want 8", code, body)
	}
	waitSweepDone(t, h, acct, 20*time.Second)
}

// The status route is how a multi-minute probe stays observable, and DELETE is
// how it stops. A second click while one is running is a 409 with instructions,
// never a silent cancellation of work this caller did not start.
func TestSweepStatusAndCancel(t *testing.T) {
	h := newHarness(t, sweepUpstream([]string{"m1", "m2", "m3", "m4", "m5", "m6"}, 150*time.Millisecond))
	acct := seededAccountID(t, h)
	path := fmt.Sprintf("/admin/accounts/%d/sync", acct)

	code, _, body := h.do("POST", path, adminTok(), `{"probe_all":true}`)
	if code != 200 {
		t.Fatalf("sync: %d (%s)", code, body)
	}

	code, _, body = h.do("GET", path, adminTok(), "")
	if code != 200 || !strings.Contains(body, `"running":true`) {
		t.Fatalf("status while sweeping: %d (%s), want running", code, body)
	}
	if !strings.Contains(body, `"probing":6`) || !strings.Contains(body, `"elapsed_s"`) {
		t.Fatalf("status must report the work and the clock: %s", body)
	}

	// A second POST must not silently cancel the first sweep.
	code, _, body = h.do("POST", path, adminTok(), `{"probe_all":true}`)
	if code != http.StatusConflict || !strings.Contains(body, "stop it first") {
		t.Fatalf("second POST: %d (%s), want 409 telling the operator to stop it", code, body)
	}

	code, _, body = h.do("DELETE", path, adminTok(), "")
	if code != http.StatusNoContent {
		t.Fatalf("cancel: %d (%s), want 204", code, body)
	}
	code, _, body = h.do("GET", path, adminTok(), "")
	if code != 200 || !strings.Contains(body, `"running":false`) {
		t.Fatalf("status after cancel: %d (%s), want not running", code, body)
	}
	code, _, body = h.do("DELETE", path, adminTok(), "")
	if code != http.StatusNotFound {
		t.Fatalf("cancel with nothing running: %d (%s), want 404", code, body)
	}
}
