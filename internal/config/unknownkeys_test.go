package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A typo in config.yaml is named at start instead of silently doing nothing;
// the shipped example has none.
func TestUnknownConfigKeysWarn(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(file, []byte("listen: \"127.0.0.1:20129\"\nrequest_limit:\n  global_inflight: 4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Warnings) != 1 || !strings.Contains(cfg.Warnings[0], "request_limit") {
		t.Fatalf("warnings = %q, want one naming request_limit", cfg.Warnings)
	}
	example, err := Load(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(example.Warnings) != 0 {
		t.Fatalf("config.example.yaml warns: %q", example.Warnings)
	}
}

func TestListenNeedsARealPort(t *testing.T) {
	for _, c := range []struct {
		listen string
		ok     bool
	}{
		{"127.0.0.1:20129", true},
		{"[::1]:20129", true},
		{":20129", true},
		{"127.0.0.1:abc", false},
		{"127.0.0.1:99999", false},
		{"127.0.0.1", false},
		{"[::1:20129", false},
	} {
		cfg := Config{Listen: c.listen, DataDir: t.TempDir()}
		err := cfg.Validate()
		got := err == nil || !strings.Contains(err.Error(), "listen")
		if got != c.ok {
			t.Errorf("listen %q: err = %v, want ok=%v", c.listen, err, c.ok)
		}
	}
}
