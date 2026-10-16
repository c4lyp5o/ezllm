package config

import (
	"os"
	"path/filepath"
	"testing"
)

// Addendum #5: the compression policy defaults and validation.
func TestCompressionPolicyDefaults(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) string {
		p := filepath.Join(dir, "c.yaml")
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if c, err := Load(write("listen: 127.0.0.1:20129\n")); err != nil {
		t.Fatal(err)
	} else if c.Compression.MinAbsoluteTokens != 24 || c.Compression.MaxPipelineMS != 250 {
		t.Errorf("defaults = %d/%dms, want 24 tokens / 250ms", c.Compression.MinAbsoluteTokens, c.Compression.MaxPipelineMS)
	}
	if c, err := Load(write("listen: 127.0.0.1:20129\ncompression:\n  min_absolute_tokens: 0\n  max_pipeline_ms: 0\n")); err != nil {
		t.Fatal(err)
	} else if c.Compression.MinAbsoluteTokens != 0 || c.Compression.MaxPipelineMS != 0 {
		t.Error("explicit 0/0 (no gates) must win over the defaults")
	}
	if _, err := Load(write("listen: 127.0.0.1:20129\ncompression:\n  max_pipeline_ms: 9000\n")); err == nil {
		t.Error("a 9000ms CPU cap must fail validation")
	}
	if _, err := Load(write("listen: 127.0.0.1:20129\ncompression:\n  min_absolute_tokens: -5\n")); err == nil {
		t.Error("a negative absolute floor must fail validation")
	}
}
