package router

// M5 — the universal wire matrix behind translation.universal_enabled.
//
// OFF must keep every routing decision exactly as it is today. ON adds the
// three NEW cross-wire cells (chat→messages, responses→chat,
// responses→messages) through the fixed preference chains, lets an explicit
// pin name the wire. A Messages destination with an unstated usage convention
// (auto) reads usage as exclusive, the same meaning the ledger gives auto.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// universalResolver builds a resolver with the matrix on and translation on
// (the universal chains are ANDed with the legacy gate).
func universalResolver(cat *fakeCatalog) *Resolver {
	return New(cat).WithTranslation(true).WithUniversal(true)
}

func resolveWire(t *testing.T, r *Resolver, target string, client provider.Surface) (provider.Surface, bool, error) {
	t.Helper()
	routes, err := r.ResolveCandidates(context.Background(), target, client, "test")
	if err != nil {
		return "", false, err
	}
	if len(routes) == 0 {
		t.Fatal("no candidates")
	}
	return routes[0].EffectiveWire(), routes[0].Translated, nil
}

// Legacy compatibility: universal OFF, a hop that is native-impossible but
// chat-capable keeps riding the single legacy alternative.
func TestUniversalOffKeepsTheSingleAlternative(t *testing.T) {
	cat := withProbes(withCombo(newFake(), singleHopCombo("gpt-6.1-sol", 2)))
	cat.noProto[probeKey(2, "gpt-6.1-sol", provider.SurfaceAnthropic)] = true
	cat.yesProto[probeKey(2, "gpt-6.1-sol", provider.SurfaceOpenAI)] = true

	wire, translated, err := resolveWire(t, New(cat).WithTranslation(true), wireCombo, provider.SurfaceAnthropic)
	if err != nil || wire != provider.SurfaceOpenAI || !translated {
		t.Fatalf("wire=%q translated=%v err=%v; want openai/true", wire, translated, err)
	}
}

// Legacy compatibility: universal OFF, responses-capable but NOT curated must
// not be auto-selected (the curated list is the only opt-in today).
func TestUniversalOffDoesNotAutoSelectResponses(t *testing.T) {
	cat := withProbes(withCombo(newFake(), singleHopCombo("gpt-6.1-sol", 2)))
	cat.noProto[probeKey(2, "gpt-6.1-sol", provider.SurfaceAnthropic)] = true
	cat.noProto[probeKey(2, "gpt-6.1-sol", provider.SurfaceOpenAI)] = true
	cat.yesProto[probeKey(2, "gpt-6.1-sol", provider.SurfaceResponses)] = true

	_, _, err := resolveWire(t, New(cat).WithTranslation(true), wireCombo, provider.SurfaceAnthropic)
	var wu *ErrWireUnsupported
	if !errors.As(err, &wu) {
		t.Fatalf("error = %v, want ErrWireUnsupported (universal off: no auto responses)", err)
	}
}

// Messages client: chat is the first preference, responses the second.
func TestUniversalMessagesClientPrefersChatThenResponses(t *testing.T) {
	cat := withProbes(withCombo(newFake(), singleHopCombo("gpt-6.1-sol", 2)))
	cat.noProto[probeKey(2, "gpt-6.1-sol", provider.SurfaceAnthropic)] = true
	cat.yesProto[probeKey(2, "gpt-6.1-sol", provider.SurfaceOpenAI)] = true
	cat.yesProto[probeKey(2, "gpt-6.1-sol", provider.SurfaceResponses)] = true

	if wire, translated, err := resolveWire(t, universalResolver(cat), wireCombo, provider.SurfaceAnthropic); err != nil || wire != provider.SurfaceOpenAI || !translated {
		t.Fatalf("wire=%q translated=%v err=%v; want chat first", wire, translated, err)
	}

	cat.noProto[probeKey(2, "gpt-6.1-sol", provider.SurfaceOpenAI)] = true
	delete(cat.yesProto, probeKey(2, "gpt-6.1-sol", provider.SurfaceOpenAI))
	if wire, translated, err := resolveWire(t, universalResolver(cat), wireCombo, provider.SurfaceAnthropic); err != nil || wire != provider.SurfaceResponses || !translated {
		t.Fatalf("wire=%q translated=%v err=%v; want responses fallback", wire, translated, err)
	}
}

// Chat client: responses first (tools+reasoning parity), messages second —
// and the messages fallback is open whatever the account's convention.
func TestUniversalChatClientPrefersResponsesThenMessages(t *testing.T) {
	cat := withProbes(withCombo(newFake(), singleHopCombo("gpt-6.1-sol", 2)))
	cat.noProto[probeKey(2, "gpt-6.1-sol", provider.SurfaceOpenAI)] = true
	cat.yesProto[probeKey(2, "gpt-6.1-sol", provider.SurfaceResponses)] = true
	cat.yesProto[probeKey(2, "gpt-6.1-sol", provider.SurfaceAnthropic)] = true

	if wire, _, err := resolveWire(t, universalResolver(cat), wireCombo, provider.SurfaceOpenAI); err != nil || wire != provider.SurfaceResponses {
		t.Fatalf("wire=%q err=%v; want responses first", wire, err)
	}

	// Responses disproved: the only remaining cell is chat→messages. An
	// unstated convention (auto) reads usage as exclusive — the ledger's own
	// meaning of auto — so it is no reason to refuse the hop.
	delete(cat.yesProto, probeKey(2, "gpt-6.1-sol", provider.SurfaceResponses))
	cat.noProto[probeKey(2, "gpt-6.1-sol", provider.SurfaceResponses)] = true

	for _, conv := range []provider.UsageConvention{provider.UsageConventionAuto, provider.UsageConventionTotal} {
		cat.accounts["super-ssn"].UsageConvention = conv
		if wire, translated, err := resolveWire(t, universalResolver(cat), wireCombo, provider.SurfaceOpenAI); err != nil || wire != provider.SurfaceAnthropic || !translated {
			t.Fatalf("convention %q: wire=%q translated=%v err=%v; want messages", conv, wire, translated, err)
		}
	}
}

// An anthropic-compatible account speaks the Messages wire by definition: a
// never-probed Messages capability on such an account is not "unproven". A
// chat client reaches it translated instead of being refused.
func TestUniversalAnthropicKindIsMessagesEvidence(t *testing.T) {
	cat := withProbes(withCombo(newFake(), singleHopCombo("gpt-6.1-sol", 2)))
	cat.accounts["super-ssn"].Kind = provider.KindAnthropicCompat
	cat.noProto[probeKey(2, "gpt-6.1-sol", provider.SurfaceOpenAI)] = true
	cat.noProto[probeKey(2, "gpt-6.1-sol", provider.SurfaceResponses)] = true

	if wire, translated, err := resolveWire(t, universalResolver(cat), wireCombo, provider.SurfaceOpenAI); err != nil || wire != provider.SurfaceAnthropic || !translated {
		t.Fatalf("wire=%q translated=%v err=%v; want messages on an anthropic-compatible account", wire, translated, err)
	}
	// A disproved Messages capability still wins over the kind.
	cat.noProto[probeKey(2, "gpt-6.1-sol", provider.SurfaceAnthropic)] = true
	if _, _, err := resolveWire(t, universalResolver(cat), wireCombo, provider.SurfaceOpenAI); err == nil {
		t.Fatal("a probe that disproved messages must not be overridden by the account kind")
	}
}

// Responses client: chat first, messages second.
func TestUniversalResponsesClientPrefersChatThenMessages(t *testing.T) {
	cat := withProbes(withCombo(newFake(), singleHopCombo("gpt-6.1-sol", 2)))
	cat.noProto[probeKey(2, "gpt-6.1-sol", provider.SurfaceResponses)] = true
	cat.yesProto[probeKey(2, "gpt-6.1-sol", provider.SurfaceOpenAI)] = true
	cat.yesProto[probeKey(2, "gpt-6.1-sol", provider.SurfaceAnthropic)] = true

	if wire, translated, err := resolveWire(t, universalResolver(cat), wireCombo, provider.SurfaceResponses); err != nil || wire != provider.SurfaceOpenAI || !translated {
		t.Fatalf("wire=%q translated=%v err=%v; want chat first", wire, translated, err)
	}

	delete(cat.yesProto, probeKey(2, "gpt-6.1-sol", provider.SurfaceOpenAI))
	cat.noProto[probeKey(2, "gpt-6.1-sol", provider.SurfaceOpenAI)] = true
	cat.accounts["super-ssn"].UsageConvention = provider.UsageConventionAnthropic
	if wire, translated, err := resolveWire(t, universalResolver(cat), wireCombo, provider.SurfaceResponses); err != nil || wire != provider.SurfaceAnthropic || !translated {
		t.Fatalf("wire=%q translated=%v err=%v; want messages fallback", wire, translated, err)
	}
}

// NULL capability is NOT translation evidence: without a proven destination
// the universal matrix refuses instead of dialing an untested wire.
func TestUniversalAlternativeRequiresProvenCapability(t *testing.T) {
	cat := withProbes(withCombo(newFake(), singleHopCombo("gpt-6.1-sol", 2)))
	cat.noProto[probeKey(2, "gpt-6.1-sol", provider.SurfaceOpenAI)] = true
	cat.accounts["super-ssn"].UsageConvention = provider.UsageConventionTotal

	_, _, err := resolveWire(t, universalResolver(cat), wireCombo, provider.SurfaceOpenAI)
	var wu *ErrWireUnsupported
	if !errors.As(err, &wu) {
		t.Fatalf("error = %v, want ErrWireUnsupported (untested destinations are not evidence)", err)
	}
}

// An explicit pin names the wire: universal mode satisfies it through that
// wire even when the client's own surface is capable.
func TestUniversalPinNamesTheWire(t *testing.T) {
	cat := withProbes(withCombo(newFake(), singleHopCombo("gpt-6.1-sol", 2)))
	cat.yesProto[probeKey(2, "gpt-6.1-sol", provider.SurfaceResponses)] = true
	cat.pins = map[string]string{"2/gpt-6.1-sol": "responses"}

	if wire, translated, err := resolveWire(t, universalResolver(cat), wireCombo, provider.SurfaceOpenAI); err != nil || wire != provider.SurfaceResponses || !translated {
		t.Fatalf("wire=%q translated=%v err=%v; want the pinned responses wire", wire, translated, err)
	}
}

// A model pinned to the Messages wire (a real Anthropic account usually is)
// serves a chat client translated, convention stated or not: the pin is the
// whole reason Hermes-style clients were turned away before.
func TestUniversalMessagesPinServesChatClients(t *testing.T) {
	cat := withProbes(withCombo(newFake(), singleHopCombo("gpt-6.1-sol", 2)))
	cat.yesProto[probeKey(2, "gpt-6.1-sol", provider.SurfaceOpenAI)] = true
	cat.yesProto[probeKey(2, "gpt-6.1-sol", provider.SurfaceAnthropic)] = true
	cat.pins = map[string]string{"2/gpt-6.1-sol": "anthropic"}

	for _, target := range []string{wireCombo, "super-ssn/gpt-6.1-sol"} {
		if wire, translated, err := resolveWire(t, universalResolver(cat), target, provider.SurfaceOpenAI); err != nil || wire != provider.SurfaceAnthropic || !translated {
			t.Fatalf("%s: wire=%q translated=%v err=%v; want the pinned messages wire", target, wire, translated, err)
		}
	}
}

// A pin onto a DISPROVED wire is refused, never dialed.
func TestUniversalPinToDisprovedWireRefused(t *testing.T) {
	cat := withProbes(withCombo(newFake(), singleHopCombo("gpt-6.1-sol", 2)))
	cat.noProto[probeKey(2, "gpt-6.1-sol", provider.SurfaceAnthropic)] = true
	cat.pins = map[string]string{"2/gpt-6.1-sol": "anthropic"}

	_, _, err := resolveWire(t, universalResolver(cat), wireCombo, provider.SurfaceOpenAI)
	var wu *ErrWireUnsupported
	if !errors.As(err, &wu) || !strings.Contains(wu.Reason, "pinned") {
		t.Fatalf("error = %v, want a refusal naming the pin", err)
	}
}

// Compatibility: with universal OFF a pin to another wire still refuses
// (a pin is a hard constraint, not a routing hint).
func TestUniversalOffKeepsPinRefusal(t *testing.T) {
	cat := withProbes(withCombo(newFake(), singleHopCombo("gpt-6.1-sol", 2)))
	cat.yesProto[probeKey(2, "gpt-6.1-sol", provider.SurfaceResponses)] = true
	cat.pins = map[string]string{"2/gpt-6.1-sol": "responses"}

	_, _, err := resolveWire(t, New(cat).WithTranslation(true), wireCombo, provider.SurfaceOpenAI)
	var np *ErrPinned
	if !errors.As(err, &np) {
		t.Fatalf("error = %v, want *ErrPinned with universal off", err)
	}
}

// universal_enabled is the SINGLE switch for the matrix: it is self-sufficient
// and does NOT require the legacy translation.enabled gate (operator decision
// 2026-10-13 — one flag, one mental model).
func TestUniversalNeedsNoOtherFlag(t *testing.T) {
	cat := withProbes(withCombo(newFake(), singleHopCombo("gpt-6.1-sol", 2)))
	cat.noProto[probeKey(2, "gpt-6.1-sol", provider.SurfaceAnthropic)] = true
	cat.yesProto[probeKey(2, "gpt-6.1-sol", provider.SurfaceOpenAI)] = true

	// No WithTranslation: universal alone must serve the cell.
	wire, translated, err := resolveWire(t, New(cat).WithUniversal(true), wireCombo, provider.SurfaceAnthropic)
	if err != nil || wire != provider.SurfaceOpenAI || !translated {
		t.Fatalf("wire=%q translated=%v err=%v; want the matrix to work on universal_enabled alone", wire, translated, err)
	}
	// And with neither flag, nothing translates (the legacy default).
	_, _, err = resolveWire(t, New(cat), wireCombo, provider.SurfaceAnthropic)
	var wu *ErrWireUnsupported
	if !errors.As(err, &wu) {
		t.Fatalf("error = %v, want ErrWireUnsupported with every flag off", err)
	}
}

// Direct and combo routes must dial the same wire for identical inputs — the
// new cells included.
func TestUniversalDirectMatchesCombo(t *testing.T) {
	cat := withProbes(withCombo(newFake(), singleHopCombo("gpt-6.1-sol", 2)))
	cat.noProto[probeKey(2, "gpt-6.1-sol", provider.SurfaceOpenAI)] = true
	cat.noProto[probeKey(2, "gpt-6.1-sol", provider.SurfaceResponses)] = true
	cat.yesProto[probeKey(2, "gpt-6.1-sol", provider.SurfaceAnthropic)] = true
	cat.accounts["super-ssn"].UsageConvention = provider.UsageConventionTotal
	r := universalResolver(cat)

	combo, _, cerr := resolveWire(t, r, wireCombo, provider.SurfaceOpenAI)
	direct, _, derr := resolveWire(t, r, "super-ssn/gpt-6.1-sol", provider.SurfaceOpenAI)
	if cerr != nil || derr != nil {
		t.Fatalf("combo err=%v direct err=%v", cerr, derr)
	}
	if combo != provider.SurfaceAnthropic || direct != combo {
		t.Fatalf("combo=%q direct=%q; want both on messages", combo, direct)
	}
}
