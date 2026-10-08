package server

// M5 end-to-end: a rules-engine refusal must surface as a 429 whose body
// carries the structured rule fields (kind + next_allowed), not a 502 and not
// a bare string. This is the contract clients rely on to sleep until a window
// reopens instead of blind-retrying.
//
// The refusal is produced by a real *rules.Refusal and travels the real path:
// resolver wraps it in *router.ErrUnavailable (cause), the server errors.As
// through to the Refusal and shapes the body. No stubs in between.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"context"
	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/proxy"
	"github.com/c4lyp5o/ezllm/internal/registration"
	"github.com/c4lyp5o/ezllm/internal/router"
	"github.com/c4lyp5o/ezllm/internal/rules"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// TestRuleRefusal429WithNextAllowed drives a direct route whose eligibility
// predicate refuses with a window Refusal, and asserts the 429 body.
func TestRuleRefusal429WithNextAllowed(t *testing.T) {
	// Minimal upstream (never reached — we refuse before forwarding).
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}]}`))
	}))
	t.Cleanup(up.Close)

	dir := t.TempDir()
	db, err := store.Open(context.Background(), store.Options{Path: dir + "/t.sqlite"})
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
	if _, _, err := db.AddKey(context.Background(), acctID, "primary", "sk-x"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertModels(context.Background(), acctID, []provider.ModelInfo{{ID: "nightly", OwnedBy: "up"}}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertClientToken(context.Background(), "hermes", "tok-infer", "infer"); err != nil {
		t.Fatal(err)
	}

	// The predicate refuses with a structured window Refusal — a night-only
	// model asked for at noon. next_allowed is a real future instant.
	next := time.Date(2026, 10, 6, 22, 0, 0, 0, time.FixedZone("+08", 8*3600))
	elig := func(ctx context.Context, accountID int64, modelID string) error {
		return &rules.Refusal{
			Requested: "up/nightly", AccountID: accountID, ModelID: modelID,
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
		strings.NewReader(`{"model":"up/nightly","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer tok-infer")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body=%s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("body not JSON: %v (%s)", err, rec.Body.String())
	}
	rule, ok := payload.Error["rule"].(map[string]any)
	if !ok {
		t.Fatalf("error.rule missing from 429 body: %v", payload.Error)
	}
	if rule["kind"] != "window" {
		t.Errorf("rule.kind = %v, want window", rule["kind"])
	}
	na, _ := rule["next_allowed"].(string)
	parsed, err := time.Parse(time.RFC3339, na)
	if err != nil || parsed.IsZero() {
		t.Errorf("rule.next_allowed = %q, want RFC3339 instant (err=%v)", na, err)
	}
}

// TestRuleRefusalCapNoNextAllowed: a cap refusal carries kind=cap but NO
// next_allowed (a cap has no window to wait for — only a reset, which the
// rule doesn't model).
func TestRuleRefusalCapNoNextAllowed(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(up.Close)
	dir := t.TempDir()
	db, err := store.Open(context.Background(), store.Options{Path: dir + "/t.sqlite"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	acctID, _ := db.UpsertAccount(context.Background(), provider.Account{
		Name: "up", Namespace: "up", Kind: provider.KindOpenAICompatible, BaseURL: up.URL, Enabled: true})
	_, _, _ = db.AddKey(context.Background(), acctID, "primary", "sk-x")
	_ = db.UpsertModels(context.Background(), acctID, []provider.ModelInfo{{ID: "capped", OwnedBy: "up"}})
	_ = db.UpsertClientToken(context.Background(), "hermes", "tok-infer", "infer")

	elig := func(ctx context.Context, accountID int64, modelID string) error {
		return &rules.Refusal{Requested: "up/capped", AccountID: accountID, ModelID: modelID,
			Reason: rules.Reason{Kind: "cap", Detail: "monthly cap reached (5000000 of 5000000 tokens)"}}
	}
	resolver := router.New(db).WithEligibility(elig)
	client := &http.Client{Timeout: 10 * time.Second}
	registry := provider.NewRegistry(client)
	s := New(Options{Auth: db, Resolver: resolver, Dispatcher: proxy.NewDispatcher(client, registry),
		DB: db, MaxBodyMiB: 1, Registry: registry, Tester: registration.NewTester(client, registry, db)})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"up/capped","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer tok-infer")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 429 {
		t.Fatalf("status = %d, want 429; body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Error map[string]any `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &payload)
	rule, _ := payload.Error["rule"].(map[string]any)
	if rule["kind"] != "cap" {
		t.Errorf("rule.kind = %v, want cap", rule["kind"])
	}
	if _, has := rule["next_allowed"]; has {
		t.Errorf("cap refusal must NOT carry next_allowed: %v", rule)
	}
}
