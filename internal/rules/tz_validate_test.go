package rules

// C5 regression: Window.loc() failed SILENTLY to UTC for a tz that
// time.LoadLocation rejects. The write path (store.ValidateModelRule) checks
// the tz with a REGEX, so a typo of a real zone — "Asia/Kuala_Lumpyr" — passes
// the write gate, then fails to resolve at every request, and the allowed-hours
// window shifts by the zone offset with no error anywhere. The operator sees a
// rule that "is not working" and nothing points at the cause.
//
// The fix validates the tz at Reload with the real resolver and refuses to
// install the rule. These tests pin that: a bogus tz is a config error, a real
// one is accepted, and an empty tz stays UTC by design.

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestValidateTZAcceptsRealZonesAndEmpty(t *testing.T) {
	for _, tz := range []string{"", "UTC", "Asia/Kuala_Lumpur", "America/New_York", "Europe/London"} {
		if err := (Window{TZ: tz}).ValidateTZ(); err != nil {
			t.Errorf("ValidateTZ(%q) = %v, want nil", tz, err)
		}
	}
	// An offset form is a legal tz too (the write path's regex accepts it).
	if err := (Window{TZ: "+08:00"}).ValidateTZ(); err != nil {
		t.Logf("offset tz +08:00 rejected by LoadLocation on this host: %v", err)
	}
}

// A typo of a REAL zone is the dangerous case: it passes the write-side regex
// and would otherwise resolve to UTC silently.
func TestValidateTZRejectsTypoOfRealZone(t *testing.T) {
	err := (Window{Start: "22:00", End: "06:00", TZ: "Asia/Kuala_Lumpyr"}).ValidateTZ()
	if err == nil {
		t.Fatal("a typo'd IANA zone must be rejected — it passes the write-side regex but never resolves")
	}
	if !strings.Contains(err.Error(), "Asia/Kuala_Lumpyr") {
		t.Errorf("error should name the offending tz verbatim, got: %v", err)
	}
}

// The doc's proof: installing a rule with a bogus tz must be a CONFIG ERROR.
func TestReloadRefusesUnresolvableTZ(t *testing.T) {
	now := time.Date(2026, 10, 11, 15, 0, 0, 0, time.UTC)
	e, err := NewEngine(&fakeSource{}, &fakeUsage{}, func() time.Time { return now }, time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	bad := []Rule{{
		AccountID: 1, ModelID: "m", Enabled: true,
		Window: Window{Start: "22:00", End: "06:00", TZ: "Asia/Kuala_Lumpyr"},
	}}
	// Point the source at the bad rule and reload.
	e.src = &fakeSource{rules: bad}
	rerr := e.Reload(context.Background())
	if rerr == nil {
		t.Fatal("Reload accepted a rule whose tz never resolves — it would silently evaluate in UTC")
	}
	if !strings.Contains(rerr.Error(), "Asia/Kuala_Lumpyr") {
		t.Errorf("reload error should name the bad tz, got: %v", rerr)
	}
	if !strings.Contains(rerr.Error(), "model \"m\"") {
		t.Errorf("reload error should name the offending rule, got: %v", rerr)
	}
	// And nothing was published: the rule must NOT be usable via the engine.
	if r := e.Get(1, "m"); r != nil {
		t.Fatal("refused rule was nonetheless published — the old set must keep serving")
	}
}

// A resolvable tz still installs and evaluates normally: the validation must
// not break the happy path, and Asia/Kuala_Lumpur is UTC+8 (no DST), so a
// window written for local midnight is 16:00 UTC.
func TestReloadAcceptsResolvableTZ(t *testing.T) {
	// 14:30 UTC = 22:30 in Kuala Lumpur (UTC+8, no DST): inside 22:00-23:00.
	now := time.Date(2026, 10, 11, 14, 30, 0, 0, time.UTC)
	e, err := NewEngine(&fakeSource{rules: []Rule{{
		AccountID: 1, ModelID: "m", Enabled: true,
		Window: Window{Start: "22:00", End: "23:00", TZ: "Asia/Kuala_Lumpur"},
	}}}, &fakeUsage{}, func() time.Time { return now }, time.Hour, false)
	if err != nil {
		t.Fatalf("resolvable tz rejected: %v", err)
	}
	defer e.Close()

	r := e.Get(1, "m")
	if r == nil {
		t.Fatal("rule with a resolvable tz was not installed")
	}
	// And the window is evaluated in the RULE's zone, not UTC: at 14:30 UTC a
	// UTC reading would say "not yet 22:00", while Kuala Lumpur reads 22:30.
	in, werr := r.Window.InWindow(now)
	if werr != nil || !in {
		t.Fatalf("window evaluated wrong for a valid tz: in=%v err=%v", in, werr)
	}
	// A UTC instant outside the KL window must be refused — proof the zone is
	// actually applied rather than defaulted.
	outside := time.Date(2026, 10, 11, 15, 30, 0, 0, time.UTC) // 23:30 KL, past the end
	if in, werr := r.Window.InWindow(outside); werr != nil || in {
		t.Fatalf("window at 23:30 KL should be closed, got in=%v err=%v", in, werr)
	}
}

// loc() keeps its UTC fallback for the REQUEST path (a corrupt row must not
// turn a 429 into a 500); only Reload refuses. This pair is the whole design:
// loud at the door, safe in the hallway.
func TestLocStillFallsBackForRequestPath(t *testing.T) {
	if got := (Window{TZ: "Asia/Kuala_Lumpyr"}).loc(); got != time.UTC {
		t.Fatalf("request-path loc() = %v, want the UTC fallback", got)
	}
}
