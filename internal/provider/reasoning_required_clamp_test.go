package provider

import "testing"

// A Required rule and a vocabulary that happens to include "off" must never
// produce a disabling effort: the two facts arrive separately, so the stored
// list can still name a value this model refuses.
//
// Measured live (Command Code, xiaomi/mimo-v2.6-pro): the vendor enumerated
// off|low|medium|high|xhigh|max, then refused "off" with "Model … does not
// support reasoning_effort \"off\". Only models that can turn thinking off
// accept it." The clamp picked off as the cheapest allowed value, turned a
// bogus client value into a guaranteed 400, and the operator read it as the
// clamp being broken.
//
// The clamp sends the cheapest NON-disabling value ("low"), not a dropped
// field: dropping hands the choice to the provider's default, which is never
// cheaper and usually medium or high.
func TestClampNeverSendsADisablingEffortWhenRequired(t *testing.T) {
	r := ReasoningRule{Allowed: []string{"off", "low", "medium", "high"}, Required: true, Known: true}

	for _, want := range []string{"off", "none", "disabled", "bogus-tier"} {
		value, drop, changed := r.Clamp(want)
		if !changed || drop || value != "low" {
			t.Errorf("Clamp(%q) = (%q, drop=%v, changed=%v), want (\"low\", false, true): the cheapest value this model accepts",
				want, value, drop, changed)
		}
	}

	// An ask below every legal value on a list with no disabling member
	// moves UP to the cheapest one rather than dropping the field.
	r2 := ReasoningRule{Allowed: []string{"low", "medium", "high"}, Required: true, Known: true}
	if value, drop, changed := r2.Clamp("minimal"); !changed || drop || value != "low" {
		t.Errorf("Clamp(minimal) = (%q, drop=%v, changed=%v), want (\"low\", false, true)", value, drop, changed)
	}

	// With no vocabulary at all there is nothing to name: drop.
	r3 := ReasoningRule{Required: true, Known: true}
	if value, drop, _ := r3.Clamp("off"); !drop || value != "" {
		t.Errorf("Clamp(off) with no vocabulary = (%q, drop=%v), want the field dropped", value, drop)
	}

	// A legal, non-disabling ask still passes through untouched.
	if value, drop, changed := r.Clamp("medium"); changed || drop || value != "medium" {
		t.Errorf("Clamp(medium) = (%q, drop=%v, changed=%v), want the request untouched", value, drop, changed)
	}
}

// The vendor's own wording for "this model cannot turn thinking off" must be
// learned as Required. Without it the rule stays "a vocabulary with no
// mandate", and the clamp keeps offering a value this model refuses.
func TestParseReasoningRequiredFromThinkingOffRefusal(t *testing.T) {
	body := []byte(`{"error":{"message":"Model \"xiaomi/mimo-v2.6-pro\" does not support reasoning_effort \"off\". Only models that can turn thinking off accept it.","type":"invalid_request_error","param":"reasoning_effort","code":"unsupported_value"}}`)
	r := ParseReasoningRule(400, body)
	if !r.Known {
		t.Fatalf("rule = %+v, want a known rule (Required)", r)
	}
	if !r.Required {
		t.Errorf("Required = false, want true: the refusal says only thinking-off models accept it, so this model requires reasoning")
	}
}
