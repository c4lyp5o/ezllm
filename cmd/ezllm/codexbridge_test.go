package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexBridgeIsOffByDefault(t *testing.T) {
	t.Setenv(envCodexBridge, "")
	if _, err := startCodexBridge(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), nil); err != nil {
		t.Fatalf("off must be a no-op: %v", err)
	}
}

// On without a usable CLI is a boot failure naming the variable, not a
// healthy-looking process whose every request 503s.
func TestCodexBridgeOnWithoutTheCLIFailsBoot(t *testing.T) {
	t.Setenv(envCodexBridge, "on")
	t.Setenv(envCodexBin, filepath.Join(t.TempDir(), "no-codex"))
	_, err := startCodexBridge(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err == nil || !strings.Contains(err.Error(), envCodexBin) {
		t.Fatalf("err = %v, want a boot failure naming %s", err, envCodexBin)
	}
}

func TestCodexBridgeRefusesANonLoopbackBind(t *testing.T) {
	for addr, ok := range map[string]bool{
		"127.0.0.1:8791": true, "[::1]:8791": true, "localhost:8791": true,
		"0.0.0.0:8791": false, "10.0.0.5:8791": false, ":8791": false, "nonsense": false,
	} {
		if err := assertCodexLoopback(addr); (err == nil) != ok {
			t.Errorf("assertCodexLoopback(%q) = %v, want ok=%v", addr, err, ok)
		}
	}
}

func TestCodexBridgeTokenPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex-bridge.token")
	first, err := codexBridgeToken(path)
	if err != nil || len(first) != 48 {
		t.Fatalf("token = %q err = %v", first, err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode = %v, want 0600", fi.Mode().Perm())
	}
	if again, _ := codexBridgeToken(path); again != first {
		t.Fatal("a second boot generated a new token: the account key would break on restart")
	}
}

// The tools policy (decision §4.7). The helper is a stub that fails its
// self-test, exactly as on a host whose kernel refuses Landlock.
func TestDecideCodexTools(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(dir, "no-landlock-helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\necho refused >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := decideCodexTools(helper, bin, false); got.status != "disabled (code-mode host missing)" || got.off == "" {
		t.Fatalf("no code-mode host: %+v", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "codex-code-mode-host"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := decideCodexTools(helper, bin, false)
	if got.status != "disabled (no Landlock)" || !strings.HasPrefix(got.off, "Landlock is unavailable (") ||
		!strings.HasPrefix(got.landlock, "unavailable: ") || got.sandbox != nil {
		t.Fatalf("no Landlock: %+v", got)
	}
	got = decideCodexTools(helper, bin, true)
	if got.status != "enabled without Landlock (operator override)" || got.off != "" || got.sandbox != nil {
		t.Fatalf("override: %+v", got)
	}
}
