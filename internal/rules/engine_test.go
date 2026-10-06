package rules

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeSource serves a fixed rule set.
type fakeSource struct{ rules []Rule }

func (f *fakeSource) AllRules(context.Context) ([]Rule, error) { return f.rules, nil }

// fakeUsage returns a canned token count (or an error) for any bucket.
type fakeUsage struct {
	tokens int64
	err    error
}

func (f *fakeUsage) UsageFor(context.Context, int64, string, string) (int64, error) {
	return f.tokens, f.err
}

// engine builds an Engine around fakes with a frozen clock — no goroutine, no
// store, no real time.
func engine(t *testing.T, now time.Time, rules []Rule, usage UsageSource) *Engine {
	t.Helper()
	e, err := NewEngine(&fakeSource{rules: rules}, usage, func() time.Time { return now }, time.Hour, false)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(e.Close)
	return e
}

func TestEvaluateNoRuleAllows(t *testing.T) {
	kl, _ := time.LoadLocation("Asia/Kuala_Lumpur")
	e := engine(t, time.Date(2026, 10, 6, 12, 0, 0, 0, kl), nil, &fakeUsage{})
	if err := e.Evaluate(context.Background(), 1, "any", "ns/any"); err != nil {
		t.Errorf("no rule must allow, got %v", err)
	}
}

func TestEvaluateWindowRefusal(t *testing.T) {
	kl, _ := time.LoadLocation("Asia/Kuala_Lumpur")
	// Night-only rule: 22:00-06:00. At 12:00 → refused with kind=window.
	r := Rule{
		AccountID: 1, ModelID: "nightly", Enabled: true,
		Window: Window{Start: "22:00", End: "06:00", TZ: "Asia/Kuala_Lumpur"},
	}
	e := engine(t, time.Date(2026, 10, 6, 12, 0, 0, 0, kl), []Rule{r}, &fakeUsage{})

	err := e.Evaluate(context.Background(), 1, "nightly", "ns/nightly")
	if err == nil {
		t.Fatal("daytime call to a night-only model must be refused")
	}
	var ref *Refusal
	if !errors.As(err, &ref) {
		t.Fatalf("want *Refusal, got %T: %v", err, err)
	}
	if ref.Reason.Kind != "window" {
		t.Errorf("kind = %q, want window", ref.Reason.Kind)
	}
	if ref.Reason.NextAllowed.IsZero() {
		t.Error("window refusal must carry next_allowed so a client can sleep until then")
	}
	if ref.Reason.NextAllowed.Hour() != 22 {
		t.Errorf("next_allowed = %v, want 22:00 today", ref.Reason.NextAllowed)
	}

	// Same rule at 23:00 → allowed (inside the window).
	e2 := engine(t, time.Date(2026, 10, 6, 23, 0, 0, 0, kl), []Rule{r}, &fakeUsage{})
	if err := e2.Evaluate(context.Background(), 1, "nightly", "ns/nightly"); err != nil {
		t.Errorf("nighttime call to a night-only model must be allowed, got %v", err)
	}
}

func TestEvaluateCapRefusal(t *testing.T) {
	kl, _ := time.LoadLocation("Asia/Kuala_Lumpur")
	r := Rule{
		AccountID: 1, ModelID: "m", Enabled: true,
		CapTokens: 1000, CapWindow: "daily",
		Window: Window{TZ: "Asia/Kuala_Lumpur"},
	}
	// Usage at cap.
	e := engine(t, time.Date(2026, 10, 6, 12, 0, 0, 0, kl), []Rule{r}, &fakeUsage{tokens: 1000})
	err := e.Evaluate(context.Background(), 1, "m", "ns/m")
	if err == nil {
		t.Fatal("a model at its cap must be refused")
	}
	var ref *Refusal
	if !errors.As(err, &ref) || ref.Reason.Kind != "cap" {
		t.Fatalf("want cap refusal, got %v", err)
	}
	if ref.Reason.NextAllowed.IsZero() == false {
		t.Error("cap refusal should not carry next_allowed (a window open time, not a reset time)")
	}

	// Under the cap → allowed.
	e2 := engine(t, time.Date(2026, 10, 6, 12, 0, 0, 0, kl), []Rule{r}, &fakeUsage{tokens: 999})
	if err := e2.Evaluate(context.Background(), 1, "m", "ns/m"); err != nil {
		t.Errorf("under-cap must be allowed, got %v", err)
	}
}

func TestEvaluateCapZeroUnlimited(t *testing.T) {
	kl, _ := time.LoadLocation("Asia/Kuala_Lumpur")
	r := Rule{AccountID: 1, ModelID: "m", Enabled: true, CapTokens: 0, Window: Window{TZ: "Asia/Kuala_Lumpur"}}
	// Even a usage source reporting a huge number must not trip CapTokens=0.
	e := engine(t, time.Date(2026, 10, 6, 12, 0, 0, 0, kl), []Rule{r}, &fakeUsage{tokens: 99_999_999})
	if err := e.Evaluate(context.Background(), 1, "m", "ns/m"); err != nil {
		t.Errorf("cap_tokens=0 means unlimited; got %v", err)
	}
}

func TestEvaluateUsageReadErrorFailsClosed(t *testing.T) {
	kl, _ := time.LoadLocation("Asia/Kuala_Lumpur")
	r := Rule{AccountID: 1, ModelID: "m", Enabled: true, CapTokens: 1000, CapWindow: "daily", Window: Window{TZ: "Asia/Kuala_Lumpur"}}
	e := engine(t, time.Date(2026, 10, 6, 12, 0, 0, 0, kl), []Rule{r}, &fakeUsage{err: errors.New("db down")})
	err := e.Evaluate(context.Background(), 1, "m", "ns/m")
	if err == nil {
		t.Fatal("a failed meter read must NOT silently un-cap (fail closed)")
	}
	var ref *Refusal
	if !errors.As(err, &ref) || ref.Reason.Kind != "cap" {
		t.Errorf("want cap refusal (meter unavailable), got %v", err)
	}
}

func TestEvaluateDisabledRuleIgnored(t *testing.T) {
	kl, _ := time.LoadLocation("Asia/Kuala_Lumpur")
	r := Rule{AccountID: 1, ModelID: "m", Enabled: false, CapTokens: 1, CapWindow: "daily",
		Window: Window{Start: "22:00", End: "06:00", TZ: "Asia/Kuala_Lumpur"}}
	e := engine(t, time.Date(2026, 10, 6, 12, 0, 0, 0, kl), []Rule{r}, &fakeUsage{tokens: 999})
	if err := e.Evaluate(context.Background(), 1, "m", "ns/m"); err != nil {
		t.Errorf("a disabled rule must impose nothing, got %v", err)
	}
}

func TestReloadReplacesSet(t *testing.T) {
	kl, _ := time.LoadLocation("Asia/Kuala_Lumpur")
	src := &fakeSource{}
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, kl)
	e, err := NewEngine(src, &fakeUsage{}, func() time.Time { return now }, time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	// No rules yet.
	if err := e.Evaluate(context.Background(), 1, "m", "ns/m"); err != nil {
		t.Errorf("empty rule set must allow, got %v", err)
	}
	// Add a night-only rule and reload → now refused at noon.
	src.rules = []Rule{{AccountID: 1, ModelID: "m", Enabled: true,
		Window: Window{Start: "22:00", End: "06:00", TZ: "Asia/Kuala_Lumpur"}}}
	if err := e.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if err := e.Evaluate(context.Background(), 1, "m", "ns/m"); err == nil {
		t.Error("after Reload the new night-only rule must be enforced")
	}
}

func TestEligibleAdaptsToRouterSignature(t *testing.T) {
	kl, _ := time.LoadLocation("Asia/Kuala_Lumpur")
	e := engine(t, time.Date(2026, 10, 6, 12, 0, 0, 0, kl), nil, &fakeUsage{})
	// Eligible must satisfy router.Eligible: func(ctx, accountID, modelID) error.
	var _ func(context.Context, int64, string) error = e.Eligible
	if err := e.Eligible(context.Background(), 1, "m"); err != nil {
		t.Errorf("no rule → allow, got %v", err)
	}
}
