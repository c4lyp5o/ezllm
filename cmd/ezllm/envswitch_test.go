package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/config"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// =false must never turn a feature on, and a typo must not pass for either.
func TestEnvSwitch(t *testing.T) {
	for _, c := range []struct {
		raw          string
		on, set, bad bool
	}{
		{"", false, false, false},
		{"  ", false, false, false},
		{"on", true, true, false},
		{"TRUE", true, true, false},
		{"1", true, true, false},
		{"off", false, true, false},
		{"OFF", false, true, false},
		{"false", false, true, false},
		{"0", false, true, false},
		{"yes", false, true, true},
		{"of", false, true, true},
	} {
		on, set, err := envSwitch("EZLLM_X", c.raw)
		if (err != nil) != c.bad || on != c.on || set != c.set {
			t.Errorf("%q: on=%v set=%v err=%v, want on=%v set=%v bad=%v", c.raw, on, set, err, c.on, c.set, c.bad)
		}
	}
}

// The three operator switches used to read anything but "off" as on.
func TestOperatorSwitchesReadFalseAsOff(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(file, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(file)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(context.Background(), store.Options{Path: filepath.Join(root, "db.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, env := range []string{"EZLLM_PRICE_REFRESH", "EZLLM_UPDATE_CHECK", "EZLLM_COMBO_AUTOPROBE"} {
		t.Setenv(env, "false")
	}
	m, err := operatorSettings(context.Background(), db, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if v := m.Current(); v.PriceRefresh || v.UpdateCheck || v.AutoProbe {
		t.Fatalf("=false left a switch on: price=%v update=%v probe=%v", v.PriceRefresh, v.UpdateCheck, v.AutoProbe)
	}
	t.Setenv("EZLLM_PRICE_REFRESH", "nope")
	if _, err := operatorSettings(context.Background(), db, cfg); err == nil || !strings.Contains(err.Error(), "EZLLM_PRICE_REFRESH") {
		t.Fatalf("a typo must stop the start naming the variable, got %v", err)
	}
}

// EZLLM_CLAUDE_BRIDGE=true used to be silently off.
func TestBridgeSwitchAcceptsTrueAndRefusesTypos(t *testing.T) {
	get := func(v string) func(string) string { return func(string) string { return v } }
	if on, err := bridgeEnabled("EZLLM_CLAUDE_BRIDGE", get("true")); err != nil || !on {
		t.Fatalf("true: on=%v err=%v", on, err)
	}
	if on, err := bridgeEnabled("EZLLM_CLAUDE_BRIDGE", get("")); err != nil || on {
		t.Fatalf("unset: on=%v err=%v", on, err)
	}
	if _, err := bridgeEnabled("EZLLM_CLAUDE_BRIDGE", get("yes")); err == nil {
		t.Fatal("yes must be refused")
	}
}
