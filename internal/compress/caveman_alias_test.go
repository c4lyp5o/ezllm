package compress

import (
	"strings"
	"testing"
)

// omniroute's aggressive/ultra are pipeline COMPOSITIONS (session_dedup +
// rtk + headroom), not caveman prose levels. The engine used to alias them
// to `standard`, so an operator asking for `ultra` silently got standard
// condensation and read the ledger as "caveman barely works".
func TestCompositionAliasIntensitiesFailOpen(t *testing.T) {
	in := "Can you explain why the router returns 502 instead of 429 here?"
	msgs := []any{
		map[string]any{"role": "user", "content": in},
		map[string]any{"role": "user", "content": "x"},
	}
	for _, alias := range []string{"aggressive", "ultra"} {
		out, fired := caveman(msgs, map[string]any{"intensity": alias})
		got := out[0].(map[string]any)["content"].(string)
		if fired != 0 {
			t.Errorf("%q must not fire any rule (fired=%d): it is not a prose level", alias, fired)
		}
		if got != in {
			t.Errorf("%q must fail open byte-identical, got %q", alias, got)
		}
	}
}

// ...while the levels the docs actually document keep working.
func TestDocumentedIntensitiesStillApply(t *testing.T) {
	in := "Can you explain why the router returns 502 instead of 429 here?"
	msgs := []any{
		map[string]any{"role": "user", "content": in},
		map[string]any{"role": "user", "content": "x"},
	}
	out, fired := caveman(msgs, map[string]any{"intensity": "standard"})
	if fired == 0 {
		t.Fatal("standard intensity must still condense prose")
	}
	if got := out[0].(map[string]any)["content"].(string); !strings.Contains(got, "Explain why") {
		t.Errorf("standard rewrite lost: %q", got)
	}
}
