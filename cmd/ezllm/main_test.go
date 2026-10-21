package main

import (
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/config"
)

// The in-process bridges dial the server's own listener. A wildcard bind
// (0.0.0.0, [::], ":port") is not an address to dial; loopback is.
func TestBridgeBaseIsLoopbackForWildcardBinds(t *testing.T) {
	for addr, want := range map[string]string{
		":20129":         "http://127.0.0.1:20129",
		"0.0.0.0:20129":  "http://127.0.0.1:20129",
		"[::]:20129":     "http://127.0.0.1:20129",
		"127.0.0.1:9000": "http://127.0.0.1:9000",
		"10.0.0.5:20129": "http://10.0.0.5:20129",
	} {
		if got := bridgeOrigin(addr); got != want {
			t.Errorf("bridgeOrigin(%q) = %q, want %q", addr, got, want)
		}
	}
}

// EZLLM_TRANSLATION_ENABLED is the one switch; the deprecated universal env
// var still rolls translation back when it is the only one set, and warns.
func TestTranslationFromEnv(t *testing.T) {
	cases := []struct {
		name          string
		env           map[string]string
		start, want   bool
		warn, wantErr bool
	}{
		{"unset keeps config", nil, true, true, false, false},
		{"new var off", map[string]string{"EZLLM_TRANSLATION_ENABLED": "false"}, true, false, false, false},
		{"new var on overrides config off", map[string]string{"EZLLM_TRANSLATION_ENABLED": "1"}, false, true, false, false},
		{"deprecated alone still rolls back", map[string]string{"EZLLM_TRANSLATION_UNIVERSAL_ENABLED": "false"}, true, false, true, false},
		{"new var wins over deprecated", map[string]string{"EZLLM_TRANSLATION_ENABLED": "true", "EZLLM_TRANSLATION_UNIVERSAL_ENABLED": "false"}, true, true, true, false},
		{"malformed new var fails", map[string]string{"EZLLM_TRANSLATION_ENABLED": "maybe"}, true, true, false, true},
		{"malformed deprecated var fails", map[string]string{"EZLLM_TRANSLATION_UNIVERSAL_ENABLED": "maybe"}, true, true, false, true},
	}
	for _, c := range cases {
		tr := config.Translation{Enabled: c.start, UniversalEnabled: c.start}
		warn, err := translationFromEnv(&tr, func(k string) string { return c.env[k] })
		if (err != nil) != c.wantErr {
			t.Fatalf("%s: err = %v, wantErr %v", c.name, err, c.wantErr)
		}
		if c.wantErr {
			continue
		}
		if tr.Enabled != c.want || tr.UniversalEnabled != c.want {
			t.Errorf("%s: enabled=%v universal=%v, want both %v", c.name, tr.Enabled, tr.UniversalEnabled, c.want)
		}
		if got := strings.Contains(warn, "deprecated"); got != c.warn {
			t.Errorf("%s: warning %q, want deprecation=%v", c.name, warn, c.warn)
		}
	}
}
