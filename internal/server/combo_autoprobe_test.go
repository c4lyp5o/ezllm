package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// A combo write probes ONLY its own enabled hop models, and only on the
// surfaces still lacking a verdict — never the account's catalog, which can
// run to 500+ models. Live 2026-10-20: command-code's minimax hop had never
// been sampled, stayed NULL, and Claude Code got its native-wire 400.
func TestComboWriteProbesOnlyUntestedHops(t *testing.T) {
	var mu sync.Mutex
	probes := map[string]int{} // model → probe requests
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		probes[body.Model]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}, func(o *Options) { o.ComboAutoProbe = true })
	acct := seededAccountID(t, h)
	ctx := context.Background()

	catalog := []provider.ModelInfo{{ID: "fresh"}, {ID: "half"}, {ID: "off"}, {ID: "proven"}, {ID: "bystander"}}
	if err := h.db.UpsertModels(ctx, acct, catalog); err != nil {
		t.Fatal(err)
	}
	yes := true
	for _, sf := range comboProbeSurfaces {
		if err := h.db.SetModelProto(ctx, acct, "proven", sf, &yes); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.db.SetModelProto(ctx, acct, "half", provider.SurfaceChat, &yes); err != nil {
		t.Fatal(err)
	}

	off := false
	code, resp := jsonDo(t, h, "POST", "/admin/combos", "tok-admin", map[string]any{
		"name": "autoprobe", "strategy": "failover", "enabled": true,
		"hops": []map[string]any{
			{"account_id": acct, "model_id": "fresh"},
			{"account_id": acct, "model_id": "half"},
			{"account_id": acct, "model_id": "off", "enabled": off},
			{"account_id": acct, "model_id": "proven"},
		},
	})
	if code != http.StatusCreated {
		t.Fatalf("create combo: %d %v", code, resp)
	}

	// The probe runs in the background: wait for its verdicts to land.
	deadline := time.Now().Add(20 * time.Second)
	for {
		done := 0
		for _, m := range listModels(t, h, acct) {
			if (m.ID == "fresh" || m.ID == "half") && m.Anthropic != nil && m.Responses != nil {
				done++
			}
		}
		if done == 2 && sweepIdle(h, acct) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("combo auto-probe did not record verdicts in time")
		}
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if probes["fresh"] != 3 {
		t.Errorf("fresh probed %d times, want 3 (every surface)", probes["fresh"])
	}
	if probes["half"] != 2 {
		t.Errorf("half probed %d times, want 2 (its openai verdict is already proven)", probes["half"])
	}
	for _, m := range []string{"off", "proven", "bystander"} {
		if probes[m] != 0 {
			t.Errorf("%s probed %d times, want 0 (disabled hop / proven / not in the combo)", m, probes[m])
		}
	}
}

// Off by default: unit-test servers and EZLLM_COMBO_AUTOPROBE=off make no
// upstream request on a combo write.
func TestComboAutoProbeOffMakesNoRequests(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
	})
	acct := seededAccountID(t, h)
	code, resp := jsonDo(t, h, "POST", "/admin/combos", "tok-admin", map[string]any{
		"name": "quiet", "strategy": "failover", "enabled": true,
		"hops": []map[string]any{{"account_id": acct, "model_id": "fresh"}},
	})
	if code != http.StatusCreated {
		t.Fatalf("create combo: %d %v", code, resp)
	}
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Errorf("upstream called %d times with auto-probe off", calls)
	}
}

func sweepIdle(h *harness, acct int64) bool {
	code, _, body := h.do("GET", fmt.Sprintf("/admin/accounts/%d/sync", acct), adminTok(), "")
	return code == 200 && json.Valid([]byte(body)) && !containsRunning(body)
}

func containsRunning(body string) bool {
	var st struct {
		Running bool `json:"running"`
	}
	_ = json.Unmarshal([]byte(body), &st)
	return st.Running
}
