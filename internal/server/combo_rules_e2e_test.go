package server

// Regression: when a combo's EVERY hop is vetoed by the rules engine, the
// refusal must surface exactly like a direct-route refusal — 429 with the
// structured rule fields — not a 502 "gateway fault".
//
// The day/night mechanism drops a rule-refused hop silently (that is what one
// combo holding a day and a night model means), but nothing-left-to-try is a
// refusal. The combo path used to bubble the raw *rules.Refusal without
// router.ErrUnavailable, so errorStatus fell through to 502.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/proxy"
	"github.com/c4lyp5o/ezllm/internal/registration"
	"github.com/c4lyp5o/ezllm/internal/router"
	"github.com/c4lyp5o/ezllm/internal/rules"
	"github.com/c4lyp5o/ezllm/internal/store"
)

func TestComboAllHopsRuleRefused429(t *testing.T) {
	// Upstream must never be reached: both hops are refused pre-forward.
	reached := false
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}]}`))
	}))
	t.Cleanup(up.Close)

	dir := t.TempDir()
	db, err := store.Open(context.Background(), store.Options{Path: dir + "/t.sqlite", MasterKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	acctID, err := db.UpsertAccount(context.Background(), provider.Account{
		Name: "up", Namespace: "up", Kind: provider.KindOpenAICompatible,
		BaseURL: up.URL, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	models := []provider.ModelInfo{{ID: "day", OwnedBy: "up"}, {ID: "night", OwnedBy: "up"}}
	if err := db.UpsertModels(context.Background(), acctID, models); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertClientToken(context.Background(), "hermes", "tok-infer", "infer"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertCombo(context.Background(), store.Combo{
		Name: "daynight", Strategy: "failover", Enabled: true,
		Hops: []store.ComboHop{
			{AccountID: acctID, ModelID: "night", Weight: 1},
			{AccountID: acctID, ModelID: "day", Weight: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Both models outside their allowed window — night-only at noon, and the
	// day model deliberately refused too, so NOTHING survives the gate.
	next := time.Date(2026, 10, 6, 22, 0, 0, 0, time.FixedZone("+08", 8*3600))
	elig := func(ctx context.Context, accountID int64, modelID string) error {
		return &rules.Refusal{
			Requested: "up/" + modelID, AccountID: accountID, ModelID: modelID,
			Reason: rules.Reason{
				Kind:        "window",
				Detail:      "allowed 22:00-06:00 Asia/Kuala_Lumpur",
				NextAllowed: next,
			},
		}
	}

	resolver := router.New(db).WithEligibility(elig)
	client := &http.Client{Timeout: 10 * time.Second}
	registry := provider.NewRegistry(client)
	s := New(Options{
		Auth: db, Resolver: resolver,
		Dispatcher: proxy.NewDispatcher(client, registry),
		DB:         db, MaxBodyMiB: 1,
		Registry: registry, Tester: registration.NewTester(client, registry, db),
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"daynight","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer tok-infer")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body=%s", rec.Code, rec.Body.String())
	}
	if reached {
		t.Error("upstream was reached despite every hop being rule-refused")
	}

	var payload struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("body is not JSON: %s", rec.Body.String())
	}
	// The top-level message names the combo; `reason` names the first hop
	// whose window refused (the actionable "who said no" line).
	msg, _ := payload.Error["message"].(string)
	if !strings.Contains(msg, "daynight") {
		t.Errorf("message = %q, want it to name the combo", msg)
	}
	reason, _ := payload.Error["reason"].(string)
	if !strings.Contains(reason, "hop up/") {
		t.Errorf("reason = %q, want a hop prefix", reason)
	}
	rule, _ := payload.Error["rule"].(map[string]any)
	if rule == nil {
		t.Fatalf("error.rule missing from combo 429 body: %v", payload.Error)
	}
	if rule["kind"] != "window" {
		t.Errorf("rule.kind = %v, want window", rule["kind"])
	}
	if na, _ := rule["next_allowed"].(string); na == "" {
		t.Errorf("rule.next_allowed missing, want RFC3339 instant")
	}
}
