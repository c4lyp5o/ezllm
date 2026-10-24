package provider

import "testing"

// The three bodies below are copied verbatim from calls in Calypso's ledger
// (ezllm.sqlite, 2026-10-20 .. 2026-10-23). They are the reason this file
// exists: every one of them rejects `reasoning_effort: "none"`, and each says
// something different about what the model WOULD accept.
const (
	bodyOpenCodeGoSpark = `{"model":"muse-spark-1.3-contributor","error":{"code":null,"message":"reasoning_effort 'none' is not supported for model 'muse-spark-1.3-contributor'. Supported values: [minimal, low, medium, high, xhigh, max]","param":"reasoning.effort","type":"invalid_request_error"}}`
	bodyCommandCode     = `{"error":{"message":"Invalid option: expected one of \"off\"|\"low\"|\"medium\"|\"high\"|\"xhigh\"|\"max\"","type":"invalid_request_error","param":"reasoning_effort"}}`
	bodyMiniMaxRequired = `{"error":{"message":"Reasoning is mandatory for this endpoint and cannot be disabled.","code":400,"metadata":{"provider_name":null}}}`
	bodyMiniMaxAdaptive = `{"error":{"type":"server_error","message":"Upstream request failed: [bad_request_error] invalid params, model \"MiniMax-M3.1-Flash\" requires adaptive thinking; reasoning effort none is not allowed (2013)"}}`
)

func TestParseReasoningRuleListsItsOwnVocabulary(t *testing.T) {
	r := ParseReasoningRule(400, []byte(bodyOpenCodeGoSpark))
	if !r.Known || len(r.Allowed) != 6 {
		t.Fatalf("rule = %+v, want 6 allowed values", r)
	}
	want := []string{"minimal", "low", "medium", "high", "xhigh", "max"}
	for i, w := range want {
		if r.Allowed[i] != w {
			t.Fatalf("allowed[%d] = %q, want %q (provider order)", i, r.Allowed[i], w)
		}
	}
	if r.Required {
		t.Fatalf("this refusal says nothing about reasoning being mandatory")
	}
}

func TestParseReasoningRuleQuotedEnumeration(t *testing.T) {
	r := ParseReasoningRule(400, []byte(bodyCommandCode))
	want := []string{"off", "low", "medium", "high"}
	if len(r.Allowed) != 6 {
		t.Fatalf("allowed = %v, want the six the provider named", r.Allowed)
	}
	for i, w := range want {
		if r.Allowed[i] != w {
			t.Fatalf("allowed = %v, want %v (provider order)", r.Allowed, want)
		}
	}
}

func TestParseReasoningRuleMandatory(t *testing.T) {
	for _, body := range []string{bodyMiniMaxRequired, bodyMiniMaxAdaptive} {
		r := ParseReasoningRule(400, []byte(body))
		if !r.Known || !r.Required {
			t.Fatalf("rule = %+v, want Known+Required for %q", r, body[:40])
		}
	}
}

func TestParseReasoningRuleIgnoresUnrelatedBodies(t *testing.T) {
	for _, body := range []string{
		`{"error":{"message":"context length exceeded"}}`,
		`{"error":{"message":"rate limit exceeded"}}`,
		`{"error":{"message":"reasoning_effort are not supported"}}`, // the FIELD, not a value
	} {
		if r := ParseReasoningRule(400, []byte(body)); r.Known {
			// The third body names the field itself — no vocabulary, no
			// mandate, so no rule: the clamp must not invent one.
			t.Fatalf("rule = %+v, want unknown for %q", r, body)
		}
	}
	// A 200 is never a refusal.
	if r := ParseReasoningRule(200, []byte(bodyMiniMaxRequired)); r.Known {
		t.Fatalf("a 200 produced a rule: %+v", r)
	}
}

// The three clamps that would have prevented the ledger's 400s.
func TestClampMapsNoneOntoWhatTheModelAccepts(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
		got  string
		drop bool
	}{
		// 'none' ranks below every allowed value → the cheapest allowed one.
		{"opencode-go", bodyOpenCodeGoSpark, "none", "minimal", false},
		// 'off' IS allowed here, and off == no reasoning, so it is exact.
		{"command-code", bodyCommandCode, "none", "off", false},
		// Required with no vocabulary: never send 'none'; let the provider's
		// own default apply.
		{"minimax-mandatory", bodyMiniMaxRequired, "none", "", true},
		{"minimax-adaptive", bodyMiniMaxAdaptive, "none", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := ParseReasoningRule(400, []byte(c.body))
			got, drop, changed := r.Clamp(c.want)
			if got != c.got || drop != c.drop {
				t.Fatalf("Clamp(%q) = (%q, drop=%t), want (%q, drop=%t)", c.want, got, drop, c.got, c.drop)
			}
			if !changed {
				t.Fatalf("a clamped value must report changed")
			}
		})
	}
}

// An allowed value must leave the body byte-identical: no clamp, no rewrite.
func TestClampLeavesAllowedValuesAlone(t *testing.T) {
	r := ParseReasoningRule(400, []byte(bodyOpenCodeGoSpark))
	for _, v := range []string{"minimal", "low", "high", "max", "MINIMAL"} {
		got, drop, changed := r.Clamp(v)
		if changed || drop || got != v {
			t.Fatalf("Clamp(%q) = (%q, drop=%t, changed=%t), want it untouched", v, got, drop, changed)
		}
	}
}

// The one invariant that matters for money: a clamp never raises effort.
func TestClampNeverRaisesEffort(t *testing.T) {
	r := ReasoningRule{Allowed: []string{"off", "low", "high"}, Known: true}
	for _, c := range []struct{ want, got string }{
		{"low", "low"},
		{"medium", "low"}, // medium is not allowed; lower, never higher
		{"xhigh", "high"}, // the highest allowed at or below the ask
		{"off", "off"},
		{"none", "off"},
		{"", ""}, // absent stays absent
	} {
		got, drop, _ := r.Clamp(c.want)
		if drop || got != c.got {
			t.Fatalf("Clamp(%q) = (%q, drop=%t), want %q", c.want, got, drop, c.got)
		}
	}
}

// An unknown rule must be inert: this is what makes deploying the clamp safe.
func TestClampIsInertWithoutARule(t *testing.T) {
	for _, r := range []ReasoningRule{{}, {Allowed: []string{"low"}}} { // second: not Known
		got, drop, changed := r.Clamp("none")
		if got != "none" || drop || changed {
			t.Fatalf("unknown rule rewrote the request: (%q, drop=%t, changed=%t)", got, drop, changed)
		}
	}
}
