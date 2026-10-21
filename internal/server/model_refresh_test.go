package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// The periodic catalog refresh reads the account the way every other path
// does — custom headers included. A gateway that needs a tenant header used
// to answer the refresh with 403 on every pass.
func TestCatalogRefreshCarriesTheFullAccount(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Tenant") != "acme" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"m1"},{"id":"m2"}]}`))
	}))
	defer up.Close()
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	id, err := h.db.UpsertAccount(ctx, provider.Account{Name: "g", Namespace: "g", Kind: provider.KindOpenAICompatible,
		BaseURL: up.URL + "/v1", Enabled: true, CustomHeaders: map[string]string{"X-Tenant": "acme"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.db.AddKey(ctx, id, "primary", "sk-test-key-123456"); err != nil {
		t.Fatal(err)
	}
	h.server.refreshCatalogsOnce(ctx)
	ok, err := h.db.ModelExists(ctx, id, "m2")
	if err != nil || !ok {
		t.Fatalf("catalog not refreshed through the tenant header: exists=%v err=%v", ok, err)
	}
}

// A refresh that lands new free models probes them right away (they cost no
// credits), never the paid ones; a free model proven broken on every surface
// leaves the auto-free combo without waiting for the next refresh.
func TestCatalogRefreshProbesFreeModelsAndPrunesBroken(t *testing.T) {
	var mu sync.Mutex
	probes := map[string]int{}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-6-luna"},{"id":"paid-model"},{"id":"tiny:free"},{"id":"broken-free"}]}`))
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		probes[body.Model]++
		mu.Unlock()
		if body.Model == "broken-free" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"model not found"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}, func(o *Options) { o.ComboAutoProbe = true })
	acct := seededAccountID(t, h)
	ctx := context.Background()

	h.server.refreshCatalogsOnce(ctx)
	// The refresh's own auto-free sync already put both free models in.
	if hops := freeComboHops(t, h); !hops["tiny:free"] || !hops["broken-free"] {
		t.Fatalf("auto-free combo after refresh = %v, want both free models", hops)
	}

	deadline := time.Now().Add(20 * time.Second)
	for {
		tested := 0
		for _, m := range listModels(t, h, acct) {
			if (m.ID == "tiny:free" || m.ID == "broken-free") && m.OpenAI != nil && m.Anthropic != nil && m.Responses != nil {
				tested++
			}
		}
		if tested == 2 && sweepIdle(h, acct) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("free models were not probed after the refresh")
		}
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	if probes["paid-model"] != 0 || probes["gpt-6-luna"] != 0 {
		t.Errorf("paid models probed (%d, %d): only free models are probed on refresh", probes["paid-model"], probes["gpt-6-luna"])
	}
	if probes["tiny:free"] != 3 {
		t.Errorf("tiny:free probed %d times, want 3 (every surface)", probes["tiny:free"])
	}
	mu.Unlock()

	hops := freeComboHops(t, h)
	if !hops["tiny:free"] {
		t.Errorf("working free model left the auto-free combo: %v", hops)
	}
	if hops["broken-free"] {
		t.Errorf("free model broken on every surface still in the auto-free combo: %v", hops)
	}

	// A second refresh re-probes nothing: every free model has its verdicts.
	mu.Lock()
	before := probes["tiny:free"] + probes["broken-free"]
	mu.Unlock()
	h.server.refreshCatalogsOnce(ctx)
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	after := probes["tiny:free"] + probes["broken-free"]
	mu.Unlock()
	if after != before {
		t.Errorf("second refresh re-probed proven free models (%d → %d probes)", before, after)
	}
}

func freeComboHops(t *testing.T, h *harness) map[string]bool {
	t.Helper()
	combos, err := h.db.ListCombos(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, c := range combos {
		if c.Name == store.DefaultFreeComboName {
			for _, hop := range c.Hops {
				out[hop.ModelID] = true
			}
			return out
		}
	}
	t.Fatalf("%s combo not seeded", store.DefaultFreeComboName)
	return nil
}

// The first pass waits bootDelay: at process start the bridge and the main
// listener are not accepting yet, so an immediate pass failed claude-sub on
// every restart.
func TestModelRefreshFirstPassWaitsForBootDelay(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"m1"}]}`))
	}))
	defer up.Close()
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	id, err := h.db.UpsertAccount(ctx, provider.Account{Name: "g", Namespace: "g", Kind: provider.KindOpenAICompatible,
		BaseURL: up.URL + "/v1", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.db.AddKey(ctx, id, "primary", "sk-test-key-123456"); err != nil {
		t.Fatal(err)
	}
	h.server.StartModelRefresh(ctx, time.Hour, 400*time.Millisecond)
	time.Sleep(150 * time.Millisecond)
	if n := hits.Load(); n != 0 {
		t.Fatalf("catalog listed %d times before the boot delay elapsed", n)
	}
	deadline := time.Now().Add(3 * time.Second)
	for hits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if hits.Load() == 0 {
		t.Fatal("first pass never ran after the boot delay")
	}
}
