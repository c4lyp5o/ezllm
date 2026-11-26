package codexbridge

import "testing"

// --sandbox is missing when help only lists --sandbox-policy or the word.
func TestMissingFlagsNeedsTheWholeFlag(t *testing.T) {
	if got := missingFlags("  --sandbox-policy <p>  runs in a sandbox\n", []string{"--sandbox"}); len(got) != 1 {
		t.Fatalf("missing = %v, want --sandbox", got)
	}
	if got := missingFlags("  -s, --sandbox <mode>\n", []string{"--sandbox"}); len(got) != 0 {
		t.Fatalf("missing = %v, want none", got)
	}
}
