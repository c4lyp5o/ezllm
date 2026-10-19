package claudebridge

import "testing"

func TestModelAliasFamilies(t *testing.T) {
	cases := []struct{ in, want string }{
		{"claude-sonnet-4-6", "sonnet"},
		{"claude-opus-5-5", "opus"},
		{"claude-haiku-5-5", "haiku"},
		{"claude-opus-5-5[1m]", "opus[1m]"},
		{"claude-sonnet-5[1m]", "sonnet[1m]"},
		{"sonnet", "sonnet"},
		{"haiku[1m]", "haiku[1m]"},
		{"CLAUDE-SONNET-4-6", "sonnet"},
		{"  opus  ", "opus"},
	}
	for _, c := range cases {
		got, err := ModelAlias(c.in)
		if err != nil {
			t.Fatalf("ModelAlias(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("ModelAlias(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestModelAliasUnknownIsAnErrorNotAFallback(t *testing.T) {
	// A client that asked for Opus must never be silently served Haiku: ids that
	// name no Claude family are refused before a process is spawned.
	for _, id := range []string{"", "gpt-4o", "deepseek-v4", "llama-3.1-8b"} {
		if got, err := ModelAlias(id); err == nil {
			t.Errorf("ModelAlias(%q) = %q, want an error (no silent fallback)", id, got)
		}
	}
}

// Dated Claude ids resolve to their family route: the subscription serves the
// pinned routes only (the official plugin maps the same way), and the response's
// `model` reports what actually ran, so the client sees the truth.
func TestModelAliasDatedClaudeIDsResolveToTheFamilyRoute(t *testing.T) {
	got, err := ModelAlias("claude-3-5-sonnet-20241022")
	if err != nil {
		t.Fatalf("ModelAlias: %v", err)
	}
	if got != "sonnet" {
		t.Errorf("got %q, want sonnet", got)
	}
}
