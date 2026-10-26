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
func TestClampNeverSendsADisablingEffortWhenRequired(t *testing.T) {
	r := ReasoningRule{Allowed: []string{"off", "low", "medium", "high"}, Required: true, Known: true}

	for _, want := range []string{"off", "none", "disabled", "bogus-tier"} {
		value, drop, changed := r.Clamp(want)
		if !changed {
			t.Errorf("Clamp(%q) changed=false: a required-reasoning model must not receive %q", want, want)
			continue
		}
		if !drop {
			t.Errorf("Clamp(%q) = %q, want the field DROPPED — nothing at or below the ask is legal here", want, value)
			continue
		}
		if value != "" {
			t.Errorf("Clamp(%q) dropped but returned value %q", want, value)
		}
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
